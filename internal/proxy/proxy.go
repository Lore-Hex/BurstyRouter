package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lore-Hex/BurstyRouter/internal/config"
	"github.com/Lore-Hex/BurstyRouter/internal/policy"
	"github.com/Lore-Hex/BurstyRouter/internal/upstream"
	trustedrouter "github.com/Lore-Hex/trusted-router-go"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	embeddingsPath      = "/v1/embeddings"
	messagesPath        = "/v1/messages"
	responsesPath       = "/v1/responses"
	modelsPath          = "/v1/models"
	catalogModelsPath   = "/models"
	trChatPath          = "/chat/completions"
	trEmbeddingsPath    = "/embeddings"
	trMessagesPath      = "/messages"
	trResponsesPath     = "/responses"
	maxInboundBodyBytes = 32 << 20
	max404SniffBytes    = 64 << 10
	catalogTimeout      = 5 * time.Second
)

type endpointFamily string

const (
	endpointChatCompletions endpointFamily = "chat_completions"
	endpointEmbeddings      endpointFamily = "embeddings"
	endpointMessages        endpointFamily = "messages"
	endpointResponses       endpointFamily = "responses"
)

type localCapableEndpoint struct {
	family    endpointFamily
	trPath    string
	localPost func(context.Context, []byte, http.Header) (*http.Response, error)
}

type localSlotResult int

const (
	localSlotAcquired localSlotResult = iota
	localSlotFull
	localSlotCanceled
)

// SavingsTotals summarizes loaded savings state for startup display.
type SavingsTotals struct {
	SavedUSDMicro      int64
	CloudSpendUSDMicro int64
	TopReference       string
	HasHistory         bool
}

// Server is the BurstyRouter HTTP proxy.
type Server struct {
	cfg        config.Config
	local      *upstream.Local
	tr         *upstream.TrustedRouter
	localSlots chan struct{}
	stats      *stats
	models     modelsCache
	catalog    *http.Client
	savings    *savingsMeter
	cloud      *cloudControl
	logStop    chan struct{}
	logDone    chan struct{}
	closeOnce  sync.Once
}

// New builds a configured proxy server.
func New(cfg config.Config) (*Server, error) {
	if cfg.LocalMaxConcurrency < 1 {
		return nil, errors.New("local max concurrency must be at least 1")
	}
	if cfg.Cloud == "" {
		cfg.Cloud = config.CloudAuto
	}

	var local *upstream.Local
	var err error
	if cfg.HasLocal() {
		local, err = upstream.NewLocal(cfg.LocalURL)
		if err != nil {
			return nil, fmt.Errorf("local upstream: %w", err)
		}
	}

	var tr *upstream.TrustedRouter
	if cfg.HasTrustedRouter() {
		tr, err = upstream.NewTrustedRouter(cfg.TRAPIKey, cfg.TRBaseURL)
		if err != nil {
			return nil, fmt.Errorf("trustedrouter upstream: %w", err)
		}
	}

	var slots chan struct{}
	if local != nil {
		slots = make(chan struct{}, cfg.LocalMaxConcurrency)
	}
	server := &Server{
		cfg:        cfg,
		local:      local,
		tr:         tr,
		localSlots: slots,
		stats:      newStats(),
		catalog:    &http.Client{Timeout: catalogTimeout},
		savings:    newSavingsMeter(cfg.StateFile),
		cloud:      newCloudControl(cfg.Cloud),
		logStop:    make(chan struct{}),
		logDone:    make(chan struct{}),
	}
	server.logSavingsSummary()
	go server.savingsLogLoop()
	return server, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.savings != nil {
		w = &savingsHeaderWriter{ResponseWriter: w, savings: s.savings}
	}
	if r.URL.Path == "/healthz" {
		s.handleHealth(w, r)
		return
	}
	if r.URL.Path == "/stats" {
		if !s.authorized(w, r) {
			return
		}
		s.handleStats(w, r)
		return
	}
	if r.URL.Path == "/metrics" {
		if !s.authorized(w, r) {
			return
		}
		s.handleMetrics(w, r)
		return
	}
	if r.URL.Path == "/ui" {
		if !s.authorized(w, r) {
			return
		}
		s.handleUI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		if !s.authorized(w, r) {
			return
		}
		w = &recentDecisionWriter{ResponseWriter: w, stats: s.stats, path: r.URL.Path}
	}

	switch r.URL.Path {
	case chatCompletionsPath:
		s.handleChat(w, r)
	case embeddingsPath:
		s.handleEmbeddings(w, r)
	case messagesPath:
		s.handleTrustedRouterOnly(w, r, endpointMessages, trMessagesPath)
	case responsesPath:
		s.handleTrustedRouterOnly(w, r, endpointResponses, trResponsesPath)
	case modelsPath:
		s.handleModels(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusNotFound, "not_found", "endpoint not found", "invalid_request_error")
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found", "invalid_request_error")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"local":         s.local != nil,
		"trustedrouter": s.tr != nil,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "invalid_request_error")
		return
	}
	payload := s.stats.snapshot()
	if s.savings != nil {
		payload["savings"] = s.savings.Snapshot(s.stats.localShare())
	}
	if s.cloud != nil {
		payload["cloud_mode"] = string(s.cloud.EffectiveMode())
	}
	if s.local != nil {
		payload["local_capacity"] = s.cfg.LocalMaxConcurrency
	} else {
		payload["local_capacity"] = 0
	}
	if s.cfg.MaxCloudSpendMicro > 0 {
		remaining := s.cfg.MaxCloudSpendMicro
		if s.savings != nil {
			remaining -= s.savings.TodayCloudSpendMicro(time.Now().UTC())
		}
		if remaining < 0 {
			remaining = 0
		}
		payload["cloud_budget_usd"] = microToUSD(s.cfg.MaxCloudSpendMicro)
		payload["cloud_budget_remaining_usd"] = microToUSD(remaining)
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.handleLocalCapable(w, r, localCapableEndpoint{
		family: endpointChatCompletions,
		trPath: trChatPath,
		localPost: func(ctx context.Context, body []byte, header http.Header) (*http.Response, error) {
			return s.local.Chat(ctx, body, header)
		},
	})
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.handleLocalCapable(w, r, localCapableEndpoint{
		family: endpointEmbeddings,
		trPath: trEmbeddingsPath,
		localPost: func(ctx context.Context, body []byte, header http.Header) (*http.Response, error) {
			return s.local.Embeddings(ctx, body, header)
		},
	})
}

func (s *Server) handleLocalCapable(w http.ResponseWriter, r *http.Request, endpoint localCapableEndpoint) {
	if r.Method != http.MethodPost {
		writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "invalid_request_error")
		return
	}
	s.stats.requestsTotal.Add(1)

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInboundBodyBytes))
	if err != nil {
		writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusBadRequest, "invalid_request_body", err.Error(), "invalid_request_error")
		return
	}
	decision, err := policy.Decide(raw, s.local != nil, s.tr != nil, policy.Options{
		Aliases:            s.cfg.Aliases,
		BurstFallbackModel: s.cfg.BurstFallbackModel,
	})
	if err != nil {
		var configErr *policy.ConfigError
		if errors.As(err, &configErr) {
			writeRoutedError(w, configErr.Route, policy.ReasonPolicy, http.StatusBadGateway, configErr.Code, configErr.Message, configErr.Type)
			return
		}
		writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusBadRequest, "invalid_request_body", err.Error(), "invalid_request_error")
		return
	}

	if decision.Reason == policy.ReasonForced {
		if decision.Route == policy.RouteLocal {
			s.stats.forcedLocal.Add(1)
		} else {
			s.stats.forcedTR.Add(1)
		}
	}

	switch decision.Route {
	case policy.RouteLocal:
		s.serveLocalCapable(w, r, decision, endpoint)
	case policy.RouteTrustedRouter:
		s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, decision.Reason, endpoint.family, decision)
	default:
		writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusInternalServerError, "internal_error", "unknown route", "api_error")
	}
}

func (s *Server) serveLocalCapable(w http.ResponseWriter, r *http.Request, decision policy.Decision, endpoint localCapableEndpoint) {
	forced := decision.Reason == policy.ReasonForced
	if s.local == nil {
		if s.tr != nil {
			if decision.Reason != policy.ReasonForced && s.cloud != nil && s.cloud.EffectiveMode() != config.CloudAuto {
				writeRoutedError(w, policy.RouteLocal, policy.ReasonPolicy, http.StatusBadGateway, "no_local_upstream", "local upstream is not configured", "api_error")
				return
			}
			s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, decision.Reason, endpoint.family, decision)
			return
		}
		writeRoutedError(w, policy.RouteLocal, policy.ReasonPolicy, http.StatusBadGateway, "no_local_upstream", "local upstream is not configured", "api_error")
		return
	}

	switch s.acquireLocalSlot(r.Context()) {
	case localSlotCanceled:
		return
	case localSlotFull:
		if !forced && s.tr != nil {
			if decision.BurstAllowed {
				switch s.allowAutomaticCloud(w, policy.ReasonBurstFull) {
				case cloudAllowed:
					s.stats.burstsFull.Add(1)
					s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstFull, endpoint.family, decision)
					return
				case cloudBlockedBudget:
					return
				case cloudBlockedMode:
				}
			}
			s.countSkippedUnmapped(decision)
		}
		w.Header().Set("Retry-After", "1")
		writeRoutedError(w, policy.RouteLocal, policy.ReasonBurstFull, http.StatusTooManyRequests, "local_overloaded", "local upstream is full", "rate_limit_error")
		return
	case localSlotAcquired:
	}
	var releaseOnce sync.Once
	releaseLocalSlot := func() {
		releaseOnce.Do(s.releaseLocalSlot)
	}
	defer releaseLocalSlot()

	localCtx, cancelLocal := context.WithCancel(r.Context())
	defer cancelLocal()

	resp, err := endpoint.localPost(localCtx, decision.LocalBody, r.Header)
	if err != nil {
		if !forced && s.cfg.BurstOnError && s.tr != nil {
			if decision.BurstAllowed {
				switch s.allowAutomaticCloud(w, policy.ReasonBurstError) {
				case cloudAllowed:
					s.stats.burstsError.Add(1)
					releaseLocalSlot()
					s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstError, endpoint.family, decision)
					return
				case cloudBlockedBudget:
					return
				case cloudBlockedMode:
				}
			}
			s.countSkippedUnmapped(decision)
		}
		writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", err.Error(), "api_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	shouldBurst, err := shouldBurstLocalResponse(resp, decision.View.Model)
	if err != nil {
		writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", err.Error(), "api_error")
		return
	}
	if shouldBurst && !forced && s.cfg.BurstOnError && s.tr != nil {
		if decision.BurstAllowed {
			switch s.allowAutomaticCloud(w, policy.ReasonBurstError) {
			case cloudAllowed:
				s.stats.burstsError.Add(1)
				closeBurstResponseBody(resp)
				releaseLocalSlot()
				s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstError, endpoint.family, decision)
				return
			case cloudBlockedBudget:
				closeBurstResponseBody(resp)
				return
			case cloudBlockedMode:
			}
		}
		s.countSkippedUnmapped(decision)
	}
	if shouldBurst && forced {
		closeBurstResponseBody(resp)
		writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", fmt.Sprintf("local upstream returned status %d", resp.StatusCode), "api_error")
		return
	}

	if forced && s.cfg.LocalSlowAfter > 0 {
		resp.Body = logSlowFirstByteReadCloser(resp.Body, s.cfg.LocalSlowAfter, decision.View.Model)
	} else if s.shouldHedgeSlowLocal(decision, resp) {
		race := startFirstBodyByteRace(resp.Body)
		body, slow, err := race.waitWithin(s.cfg.LocalSlowAfter)
		if err != nil {
			writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", err.Error(), "api_error")
			return
		}
		resp.Body = body
		if slow {
			switch s.allowAutomaticCloud(w, policy.ReasonBurstSlow) {
			case cloudAllowed:
				s.stats.burstsSlow.Add(1)
				cancelLocal()
				closeBurstResponseBody(resp)
				releaseLocalSlot()
				s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstSlow, endpoint.family, decision)
				return
			case cloudBlockedBudget:
				cancelLocal()
				closeBurstResponseBody(resp)
				releaseLocalSlot()
				return
			case cloudBlockedMode:
				body, err := race.wait()
				if err != nil {
					writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", err.Error(), "api_error")
					return
				}
				resp.Body = body
			}
		}
	}

	s.serveUpstreamResponse(w, r, resp, policy.RouteLocal, decision.Reason, decision.View.Stream, endpoint.family, decision)
}

type cloudAllowance int

const (
	cloudAllowed cloudAllowance = iota
	cloudBlockedMode
	cloudBlockedBudget
)

func (s *Server) allowAutomaticCloud(w http.ResponseWriter, reason policy.Reason) cloudAllowance {
	if s.cloud != nil && s.cloud.EffectiveMode() != config.CloudAuto {
		s.stats.cloudBlockedMode.Add(1)
		return cloudBlockedMode
	}
	if s.writeCloudBudgetBlockIfNeeded(w, reason) {
		return cloudBlockedBudget
	}
	return cloudAllowed
}

func (s *Server) serveTrustedRouterRaw(w http.ResponseWriter, r *http.Request, path string, body []byte, requestStream bool, reason policy.Reason, endpoint endpointFamily, decision policy.Decision) {
	explicit := reason == policy.ReasonForced || isTrustedRouterOnlyEndpoint(endpoint)
	if s.writeCloudModeBlockIfNeeded(w, reason, explicit) {
		return
	}
	if s.writeCloudBudgetBlockIfNeeded(w, reason) {
		return
	}
	if s.tr == nil {
		w.Header().Set("Retry-After", "1")
		writeRoutedError(w, policy.RouteLocal, reason, http.StatusTooManyRequests, "local_overloaded", "TrustedRouter is not configured", "rate_limit_error")
		return
	}
	resp, err := s.tr.RawPost(r.Context(), path, body, r.Header)
	if err != nil {
		if passthroughEndpointNotFound(endpoint, err) {
			writePassthroughUnsupported(w, r.URL.Path, reason)
			return
		}
		status, message := trustedRouterError(err)
		writeRoutedError(w, policy.RouteTrustedRouter, reason, status, "trustedrouter_upstream_error", message, "api_error")
		return
	}
	defer resp.Body.Close()
	if isTrustedRouterOnlyEndpoint(endpoint) && resp.StatusCode == http.StatusNotFound {
		drainAndClose(resp.Body)
		writePassthroughUnsupported(w, r.URL.Path, reason)
		return
	}
	s.serveUpstreamResponse(w, r, resp, policy.RouteTrustedRouter, reason, requestStream, endpoint, decision)
}

func (s *Server) writeCloudModeBlockIfNeeded(w http.ResponseWriter, reason policy.Reason, explicit bool) bool {
	if s.cloud == nil {
		return false
	}
	mode := s.cloud.EffectiveMode()
	switch mode {
	case config.CloudAuto:
		return false
	case config.CloudExplicit:
		if explicit {
			return false
		}
		s.stats.cloudBlockedMode.Add(1)
		writeRoutedError(w, policy.RouteTrustedRouter, reason, http.StatusTooManyRequests, "cloud_disabled", "cloud disabled by -cloud=explicit", "rate_limit_error")
		return true
	case config.CloudOff:
		s.stats.cloudBlockedMode.Add(1)
		writeRoutedError(w, policy.RouteTrustedRouter, reason, http.StatusServiceUnavailable, "cloud_disabled", "cloud disabled by -cloud=off", "api_error")
		return true
	default:
		return false
	}
}

func (s *Server) writeCloudBudgetBlockIfNeeded(w http.ResponseWriter, reason policy.Reason) bool {
	if s.savings == nil || s.cfg.MaxCloudSpendMicro <= 0 {
		return false
	}
	now := time.Now().UTC()
	if !s.savings.BudgetExhausted(s.cfg.MaxCloudSpendMicro, now) {
		return false
	}
	s.stats.cloudBlockedBudget.Add(1)
	if s.cloud != nil {
		s.cloud.LogBudgetBlockOnce(now, s.cfg.MaxCloudSpendMicro)
	}
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfterUTCMidnight(now), 10))
	writeRoutedError(w, policy.RouteTrustedRouter, reason, http.StatusTooManyRequests, "cloud_budget_exhausted", "daily cloud spend budget exhausted", "cloud_budget_exhausted")
	return true
}

func (s *Server) handleTrustedRouterOnly(w http.ResponseWriter, r *http.Request, endpoint endpointFamily, path string) {
	if r.Method != http.MethodPost {
		writeRoutedError(w, policy.RouteTrustedRouter, policy.ReasonPolicy, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "invalid_request_error")
		return
	}
	s.stats.requestsTotal.Add(1)

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInboundBodyBytes))
	if err != nil {
		writeRoutedError(w, policy.RouteTrustedRouter, policy.ReasonPolicy, http.StatusBadRequest, "invalid_request_body", err.Error(), "invalid_request_error")
		return
	}
	decision, err := policy.DecideTrustedRouterOnly(raw)
	if err != nil {
		writeRoutedError(w, policy.RouteTrustedRouter, policy.ReasonPolicy, http.StatusBadRequest, "invalid_request_body", err.Error(), "invalid_request_error")
		return
	}
	if decision.Route == policy.RouteLocal {
		writeRoutedError(w, policy.RouteLocal, policy.ReasonForced, http.StatusBadRequest, "endpoint_not_supported", fmt.Sprintf("%s cannot be served by a local OpenAI-compatible upstream", r.URL.Path), "invalid_request_error")
		return
	}
	if s.tr == nil {
		writeRoutedError(w, policy.RouteTrustedRouter, decision.Reason, http.StatusNotImplemented, "endpoint_not_supported", fmt.Sprintf("%s requires TrustedRouter; local-only mode cannot serve this endpoint", r.URL.Path), "invalid_request_error")
		return
	}
	if decision.Reason == policy.ReasonForced {
		s.stats.forcedTR.Add(1)
	}
	s.serveTrustedRouterRaw(w, r, path, decision.TRBody, decision.View.Stream, decision.Reason, endpoint, decision)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "invalid_request_error")
		return
	}
	s.stats.requestsTotal.Add(1)

	merged := make([]map[string]any, 0)
	if s.tr != nil {
		if models, err := s.cachedTrustedRouterModels(r.Context()); err == nil {
			merged = append(merged, models...)
		}
	}
	if s.local != nil {
		if models, err := s.localModels(r.Context()); err == nil {
			merged = append(merged, models...)
		}
	}
	merged = append(merged, aliasModels(s.cfg.Aliases)...)

	route := policy.RouteLocal
	if s.tr != nil {
		route = policy.RouteTrustedRouter
	}
	setRouteHeaders(w, route, policy.ReasonPolicy)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   merged,
	})
}

func (s *Server) defaultRoute() policy.Route {
	if s.local != nil {
		return policy.RouteLocal
	}
	return policy.RouteTrustedRouter
}

// Close flushes persistent savings state and stops background proxy goroutines.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		if s.logStop != nil {
			close(s.logStop)
			<-s.logDone
		}
		s.logSavingsSummary()
		if s.savings != nil {
			s.savings.Close()
		}
	})
}

// HandleSIGHUP toggles cloud egress between the configured mode and off.
func (s *Server) HandleSIGHUP() {
	if s.cloud != nil {
		s.cloud.HandleSIGHUP()
	}
}

// SavingsTotals returns loaded savings totals for startup display.
func (s *Server) SavingsTotals() SavingsTotals {
	if s.savings == nil {
		return SavingsTotals{}
	}
	saved, cloudSpend, ref := s.savings.Totals()
	return SavingsTotals{
		SavedUSDMicro:      saved,
		CloudSpendUSDMicro: cloudSpend,
		TopReference:       ref,
		HasHistory:         s.savings.HasHistory(),
	}
}

func (s *Server) savingsLogLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	defer close(s.logDone)
	for {
		select {
		case <-ticker.C:
			s.logSavingsSummary()
		case <-s.logStop:
			return
		}
	}
}

func (s *Server) logSavingsSummary() {
	if s.savings == nil || s.stats == nil {
		return
	}
	saved, cloudSpend, ref := s.savings.Totals()
	log.Printf("bursty savings: served %.0f%% locally, saved $%s (ref: %s), cloud spend $%s", s.stats.localShare()*100, formatUSDLog(saved), ref, formatUSDLog(cloudSpend))
}

func (s *Server) acquireLocalSlot(ctx context.Context) localSlotResult {
	if s.localSlots == nil {
		return localSlotFull
	}
	select {
	case <-ctx.Done():
		return localSlotCanceled
	default:
	}
	if s.cfg.LocalQueueWait == 0 {
		select {
		case s.localSlots <- struct{}{}:
			s.stats.inFlightLocal.Add(1)
			return localSlotAcquired
		default:
			return localSlotFull
		case <-ctx.Done():
			return localSlotCanceled
		}
	}
	timer := time.NewTimer(s.cfg.LocalQueueWait)
	defer timer.Stop()
	select {
	case s.localSlots <- struct{}{}:
		s.stats.inFlightLocal.Add(1)
		return localSlotAcquired
	case <-timer.C:
		return localSlotFull
	case <-ctx.Done():
		return localSlotCanceled
	}
}

func (s *Server) releaseLocalSlot() {
	<-s.localSlots
	s.stats.inFlightLocal.Add(-1)
}

func (s *Server) countSkippedUnmapped(decision policy.Decision) {
	if decision.BurstSkippedUnmapped {
		s.stats.burstsSkippedUnmapped.Add(1)
	}
}

func (s *Server) shouldHedgeSlowLocal(decision policy.Decision, resp *http.Response) bool {
	if s.cfg.LocalSlowAfter <= 0 || s.tr == nil || !decision.BurstAllowed {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if s.cloud != nil && s.cloud.EffectiveMode() != config.CloudAuto {
		return false
	}
	return !s.cloudBudgetExhausted()
}

func (s *Server) cloudBudgetExhausted() bool {
	if s.savings == nil || s.cfg.MaxCloudSpendMicro <= 0 {
		return false
	}
	return s.savings.BudgetExhausted(s.cfg.MaxCloudSpendMicro, time.Now().UTC())
}

func shouldBurstLocalResponse(resp *http.Response, model string) (bool, error) {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		body, err := io.ReadAll(io.LimitReader(resp.Body, max404SniffBytes))
		if err != nil {
			return false, err
		}
		resp.Body = readCloser{
			Reader: io.MultiReader(bytes.NewReader(body), resp.Body),
			Closer: resp.Body,
		}
		return bodyMentionsModelNotFound(body, model), nil
	default:
		return false, nil
	}
}

func bodyMentionsModelNotFound(body []byte, model string) bool {
	if message, ok := jsonErrorMessage(body); ok {
		return containsModelCandidate(message, model, 0)
	}
	return containsModelCandidate(string(body), model, 3)
}

func jsonErrorMessage(body []byte) (string, bool) {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Error.Message == "" {
		return "", false
	}
	return payload.Error.Message, true
}

func containsModelCandidate(haystack, model string, minLen int) bool {
	lower := strings.ToLower(haystack)
	for _, candidate := range modelCandidates(model) {
		if candidate != "" && len(candidate) >= minLen && strings.Contains(lower, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

func modelCandidates(model string) []string {
	if strings.HasPrefix(model, "local/") {
		return []string{model, strings.TrimPrefix(model, "local/")}
	}
	return []string{model}
}

func (s *Server) serveUpstreamResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, route policy.Route, reason policy.Reason, requestStream bool, endpoint endpointFamily, decision policy.Decision) {
	s.stats.countEndpointRoute(endpoint, route)
	copyResponseHeaders(w.Header(), resp.Header)
	setRouteHeaders(w, route, reason)

	streaming := requestStream || strings.Contains(strings.ToLower(resp.Header.Get("content-type")), "text/event-stream")
	if !streaming && isJSONResponse(resp.Header) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			w.Header().Del("Content-Length")
			w.Header().Del("Content-Encoding")
			writeRoutedError(w, route, reason, http.StatusBadGateway, "upstream_read_error", err.Error(), "api_error")
			return
		}
		capture := usageCapture{}
		record := savingsRecord{}
		if shouldCaptureUsage(resp) {
			capture = extractUsageAndModel(body)
			record = s.recordResponseUsage(r.Context(), route, decision, capture, false)
		}
		if route == policy.RouteTrustedRouter {
			s.logCloudCompletion(reason, decision, capture, record)
		}
		if len(bytes.TrimSpace(body)) > 0 {
			injected, err := injectBurstyBlock(body, route, reason)
			if err == nil {
				body = injected
				w.Header().Del("Content-Encoding")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			} else {
				w.Header().Del("Content-Length")
				w.Header().Del("Content-Encoding")
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	w.Header().Del("Content-Length")
	var scanner *streamUsageScanner
	if shouldCaptureUsage(resp) {
		scanner = &streamUsageScanner{}
	}
	streamBody(w, resp, scanner)
	capture := usageCapture{}
	record := savingsRecord{}
	if scanner != nil {
		capture = scanner.Finish()
		record = s.recordResponseUsage(r.Context(), route, decision, capture, true)
	}
	if route == policy.RouteTrustedRouter {
		s.logCloudCompletion(reason, decision, capture, record)
	}
}

func streamBody(w http.ResponseWriter, resp *http.Response, scanner *streamUsageScanner) {
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, 32*1024)
	writer := io.Writer(flushWriter{w: w, flusher: flusher})
	if scanner != nil {
		writer = usageScanningWriter{dst: writer, scanner: scanner}
	}
	_, _ = io.CopyBuffer(writer, resp.Body, buf)
}

type flushWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

type savingsHeaderWriter struct {
	http.ResponseWriter
	savings *savingsMeter
	wrote   bool
}

func (w *savingsHeaderWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		if w.savings != nil && w.Header().Get("X-Bursty-Route") != "" {
			w.Header().Set("X-Bursty-Saved-USD", w.savings.SavedUSDHeader())
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *savingsHeaderWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *savingsHeaderWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func injectBurstyBlock(body []byte, route policy.Route, reason policy.Reason) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{
		"route":  string(route),
		"reason": string(reason),
	})
	if err != nil {
		return nil, err
	}
	return policy.InjectTopLevelObject(body, "bursty", payload)
}

func (s *Server) recordResponseUsage(ctx context.Context, route policy.Route, decision policy.Decision, capture usageCapture, streaming bool) savingsRecord {
	if s.savings == nil {
		return savingsRecord{}
	}
	if !capture.HasUsage {
		if streaming {
			s.savings.RecordUnknownUsage()
		}
		return savingsRecord{}
	}
	switch route {
	case policy.RouteLocal:
		return s.savings.RecordLocalUsage(capture.Usage, s.localSavingsPrice(ctx, decision))
	case policy.RouteTrustedRouter:
		model := responseModel(capture, decision.View.Model)
		return s.savings.RecordCloudUsage(capture.Usage, s.cloudSavingsPrice(ctx, model))
	default:
		return savingsRecord{}
	}
}

func (s *Server) logCloudCompletion(reason policy.Reason, decision policy.Decision, capture usageCapture, record savingsRecord) {
	model := responseModel(capture, decision.View.Model)
	if model == "" {
		model = "unknown"
	}
	promptTokens := capture.Usage.PromptTokens
	completionTokens := capture.Usage.CompletionTokens
	if record.Priced {
		log.Printf("bursty cloud: reason=%s model=%s prompt_toks=%d completion_toks=%d est_cost=$%s", reason, model, promptTokens, completionTokens, formatUSDBurst(record.CostMicro))
		return
	}
	log.Printf("bursty cloud: reason=%s model=%s prompt_toks=%d completion_toks=%d", reason, model, promptTokens, completionTokens)
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	bearerOK := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.cfg.Token))
	apiKeyOK := subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), []byte(s.cfg.Token))
	if bearerOK|apiKeyOK == 1 {
		return true
	}
	writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token or x-api-key", "authentication_error")
	return false
}

func trustedRouterError(err error) (int, string) {
	var trErr *trustedrouter.Error
	if errors.As(err, &trErr) {
		status := trErr.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		return status, trErr.Message
	}
	return http.StatusBadGateway, err.Error()
}

func passthroughEndpointNotFound(endpoint endpointFamily, err error) bool {
	if !isTrustedRouterOnlyEndpoint(endpoint) {
		return false
	}
	var notFound *trustedrouter.NotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	var trErr *trustedrouter.Error
	return errors.As(err, &trErr) && trErr.StatusCode == http.StatusNotFound
}

func isTrustedRouterOnlyEndpoint(endpoint endpointFamily) bool {
	return endpoint == endpointMessages || endpoint == endpointResponses
}

func writePassthroughUnsupported(w http.ResponseWriter, path string, reason policy.Reason) {
	writeRoutedError(w, policy.RouteTrustedRouter, reason, http.StatusNotImplemented, "endpoint_not_supported", fmt.Sprintf("%s is not supported by the configured burst upstream", path), "invalid_request_error")
}

func copyResponseHeaders(dst, src http.Header) {
	dynamicHopByHop := connectionHeaderTokens(src)
	for key, values := range src {
		if shouldDropResponseHeader(key, dynamicHopByHop) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldDropResponseHeader(key string, dynamicHopByHop map[string]struct{}) bool {
	lower := strings.ToLower(key)
	if _, ok := dynamicHopByHop[lower]; ok {
		return true
	}
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te",
		"trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func connectionHeaderTokens(header http.Header) map[string]struct{} {
	out := map[string]struct{}{}
	for _, value := range header.Values("Connection") {
		for _, part := range strings.Split(value, ",") {
			token := strings.ToLower(strings.TrimSpace(part))
			if token != "" {
				out[token] = struct{}{}
			}
		}
	}
	return out
}

func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

func closeBurstResponseBody(resp *http.Response) {
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		drainAndClose(resp.Body)
		return
	}
	_ = resp.Body.Close()
}

type firstBodyByteRace struct {
	body io.ReadCloser
	done chan firstBodyByteRead
}

type firstBodyByteRead struct {
	buf [1]byte
	n   int
	err error
}

func startFirstBodyByteRace(body io.ReadCloser) *firstBodyByteRace {
	race := &firstBodyByteRace{
		body: body,
		done: make(chan firstBodyByteRead, 1),
	}
	go func() {
		var buf [1]byte
		n, err := body.Read(buf[:])
		race.done <- firstBodyByteRead{buf: buf, n: n, err: err}
	}()
	return race
}

func (r *firstBodyByteRace) waitWithin(timeout time.Duration) (io.ReadCloser, bool, error) {
	if timeout <= 0 {
		return r.body, false, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-r.done:
		body, err := r.bodyAfter(result)
		return body, false, err
	case <-timer.C:
		return r.body, true, nil
	}
}

func (r *firstBodyByteRace) wait() (io.ReadCloser, error) {
	result := <-r.done
	return r.bodyAfter(result)
}

func (r *firstBodyByteRace) bodyAfter(result firstBodyByteRead) (io.ReadCloser, error) {
	if result.n > 0 {
		return readCloser{
			Reader: io.MultiReader(bytes.NewReader(result.buf[:result.n]), r.body),
			Closer: r.body,
		}, nil
	}
	if result.err != nil && !errors.Is(result.err, io.EOF) {
		return r.body, result.err
	}
	return r.body, nil
}

type firstByteLogReadCloser struct {
	io.ReadCloser
	done  chan struct{}
	once  sync.Once
	timer *time.Timer
}

func logSlowFirstByteReadCloser(body io.ReadCloser, timeout time.Duration, model string) io.ReadCloser {
	wrapped := &firstByteLogReadCloser{
		ReadCloser: body,
		done:       make(chan struct{}),
	}
	wrapped.timer = time.AfterFunc(timeout, func() {
		select {
		case <-wrapped.done:
		default:
			log.Printf("bursty local: forced local first byte exceeded %s for model=%s; waiting for local", timeout, model)
		}
	})
	return wrapped
}

func (r *firstByteLogReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 || err != nil {
		r.markDone()
	}
	return n, err
}

func (r *firstByteLogReadCloser) Close() error {
	r.markDone()
	return r.ReadCloser.Close()
}

func (r *firstByteLogReadCloser) markDone() {
	r.once.Do(func() {
		close(r.done)
		if r.timer != nil {
			r.timer.Stop()
		}
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

func isJSONResponse(header http.Header) bool {
	contentType := strings.ToLower(header.Get("content-type"))
	return strings.Contains(contentType, "application/json") || strings.Contains(contentType, "+json")
}

func setRouteHeaders(w http.ResponseWriter, route policy.Route, reason policy.Reason) {
	w.Header().Set("X-Bursty-Route", string(route))
	w.Header().Set("X-Bursty-Reason", string(reason))
}

func writeRoutedError(w http.ResponseWriter, route policy.Route, reason policy.Reason, status int, code, message, typ string) {
	setRouteHeaders(w, route, reason)
	writeError(w, status, code, message, typ)
}

func writeError(w http.ResponseWriter, status int, code, message, typ string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"type":    typ,
			"source":  "bursty",
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type stats struct {
	inFlightLocal         expvar.Int
	burstsFull            expvar.Int
	burstsError           expvar.Int
	burstsSlow            expvar.Int
	burstsSkippedUnmapped expvar.Int
	forcedLocal           expvar.Int
	forcedTR              expvar.Int
	requestsTotal         expvar.Int
	catalogErrors         expvar.Int
	cloudBlockedBudget    expvar.Int
	cloudBlockedMode      expvar.Int
	routes                routeStats
	chatRoutes            routeStats
	embeddingRoutes       routeStats
	messageRoutes         routeStats
	responseRoutes        routeStats
	recent                recentDecisions
}

func newStats() *stats {
	return &stats{}
}

func (s *stats) countEndpointRoute(endpoint endpointFamily, route policy.Route) {
	s.routes.count(route)
	switch endpoint {
	case endpointChatCompletions:
		s.chatRoutes.count(route)
	case endpointEmbeddings:
		s.embeddingRoutes.count(route)
	case endpointMessages:
		s.messageRoutes.count(route)
	case endpointResponses:
		s.responseRoutes.count(route)
	}
}

type routeStats struct {
	local expvar.Int
	tr    expvar.Int
}

func (s *routeStats) count(route policy.Route) {
	if route == policy.RouteLocal {
		s.local.Add(1)
		return
	}
	if route == policy.RouteTrustedRouter {
		s.tr.Add(1)
	}
}

func (s *routeStats) snapshot() map[string]any {
	return map[string]any{
		"local":         s.local.Value(),
		"trustedrouter": s.tr.Value(),
	}
}

func (s *stats) snapshot() map[string]any {
	return map[string]any{
		"in_flight_local":         s.inFlightLocal.Value(),
		"bursts_full":             s.burstsFull.Value(),
		"bursts_error":            s.burstsError.Value(),
		"bursts_slow":             s.burstsSlow.Value(),
		"bursts_skipped_unmapped": s.burstsSkippedUnmapped.Value(),
		"forced_local":            s.forcedLocal.Value(),
		"forced_tr":               s.forcedTR.Value(),
		"requests_total":          s.requestsTotal.Value(),
		"catalog_errors":          s.catalogErrors.Value(),
		"cloud_blocked_budget":    s.cloudBlockedBudget.Value(),
		"cloud_blocked_mode":      s.cloudBlockedMode.Value(),
		"routes":                  s.routes.snapshot(),
		"endpoint_routes": map[string]any{
			string(endpointChatCompletions): s.chatRoutes.snapshot(),
			string(endpointEmbeddings):      s.embeddingRoutes.snapshot(),
			string(endpointMessages):        s.messageRoutes.snapshot(),
			string(endpointResponses):       s.responseRoutes.snapshot(),
		},
		"recent": s.recent.snapshot(recentDecisionCap),
	}
}

func (s *stats) localShare() float64 {
	local := s.routes.local.Value()
	tr := s.routes.tr.Value()
	total := local + tr
	if total == 0 {
		return 0
	}
	return float64(local) / float64(total)
}

type modelsCache struct {
	mu      sync.Mutex
	expires time.Time
	data    []map[string]any
	hasData bool
}

func (s *Server) cachedTrustedRouterModels(ctx context.Context) ([]map[string]any, error) {
	now := time.Now()
	s.models.mu.Lock()
	if s.models.hasData && now.Before(s.models.expires) {
		data := cloneModels(s.models.data)
		s.models.mu.Unlock()
		return data, nil
	}
	stale := cloneModels(s.models.data)
	hasStale := s.models.hasData
	s.models.mu.Unlock()

	data, err := s.fetchTrustedRouterModelMaps(ctx)
	if err != nil {
		s.stats.catalogErrors.Add(1)
		if hasStale {
			return stale, nil
		}
		return nil, err
	}

	s.models.mu.Lock()
	s.models.data = cloneModels(data)
	s.models.expires = now.Add(60 * time.Second)
	s.models.hasData = true
	s.models.mu.Unlock()
	return data, nil
}

func (s *Server) fetchTrustedRouterModelMaps(ctx context.Context) ([]map[string]any, error) {
	if s.tr != nil {
		list, err := s.tr.Models(ctx)
		if err == nil {
			return trustedModelsToMaps(list)
		}
		if !isTrustedRouterNotFound(err) {
			return nil, err
		}
	}
	// The SDK default reaches TrustedRouter's attested API plane, where /v1/models
	// can be absent; the public model catalog lives on the control plane.
	return s.fetchControlPlaneCatalog(ctx)
}

func (s *Server) fetchControlPlaneCatalog(ctx context.Context) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.catalogModelsURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.catalogClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("trustedrouter catalog status %d", resp.StatusCode)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Data == nil {
		return []map[string]any{}, nil
	}
	return payload.Data, nil
}

func (s *Server) catalogClient() *http.Client {
	if s.catalog != nil {
		return s.catalog
	}
	return &http.Client{Timeout: catalogTimeout}
}

func (s *Server) catalogModelsURL() string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.TRCatalogURL), "/")
	if baseURL == "" {
		baseURL = config.DefaultTRCatalogURL
	}
	return baseURL + catalogModelsPath
}

func isTrustedRouterNotFound(err error) bool {
	var notFound *trustedrouter.NotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	var trErr *trustedrouter.Error
	return errors.As(err, &trErr) && trErr.StatusCode == http.StatusNotFound
}

func (s *Server) localModels(ctx context.Context) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := s.local.Models(ctx)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("local models status %d", resp.StatusCode)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(payload.Data)*2)
	for _, model := range payload.Data {
		id, _ := model["id"].(string)
		if id == "" {
			continue
		}
		bare := cloneModel(model)
		bare["id"] = id
		bare["owned_by"] = "local"
		if _, ok := bare["object"]; !ok {
			bare["object"] = "model"
		}
		prefixed := cloneModel(bare)
		prefixed["id"] = "local/" + id
		out = append(out, bare, prefixed)
	}
	return out, nil
}

func aliasModels(aliases map[string]string) []map[string]any {
	if len(aliases) == 0 {
		return nil
	}
	keys := make([]string, 0, len(aliases))
	for key := range aliases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{
			"id":       key,
			"object":   "model",
			"owned_by": "bursty-alias",
			"metadata": map[string]any{
				"local_target": aliases[key],
			},
		})
	}
	return out
}

func trustedModelsToMaps(list *trustedrouter.ModelList) ([]map[string]any, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]map[string]any, 0, len(list.Data))
	for _, model := range list.Data {
		body, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		var mapped map[string]any
		if err := json.Unmarshal(body, &mapped); err != nil {
			return nil, err
		}
		out = append(out, mapped)
	}
	return out, nil
}

func cloneModels(in []map[string]any) []map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make([]map[string]any, len(in))
	for i := range in {
		out[i] = cloneModel(in[i])
	}
	return out
}

func cloneModel(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
