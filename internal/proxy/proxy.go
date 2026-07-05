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
	"net/http"
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

// Server is the BurstyRouter HTTP proxy.
type Server struct {
	cfg        config.Config
	local      *upstream.Local
	tr         *upstream.TrustedRouter
	localSlots chan struct{}
	stats      *stats
	models     modelsCache
	catalog    *http.Client
}

// New builds a configured proxy server.
func New(cfg config.Config) (*Server, error) {
	if cfg.LocalMaxConcurrency < 1 {
		return nil, errors.New("local max concurrency must be at least 1")
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
	return &Server{
		cfg:        cfg,
		local:      local,
		tr:         tr,
		localSlots: slots,
		stats:      newStats(),
		catalog:    &http.Client{Timeout: catalogTimeout},
	}, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if strings.HasPrefix(r.URL.Path, "/v1/") && !s.authorized(w, r) {
		return
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
	writeJSON(w, http.StatusOK, s.stats.snapshot())
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
	decision, err := policy.Decide(raw, s.local != nil, s.tr != nil)
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
		s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, decision.Reason, endpoint.family)
	default:
		writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusInternalServerError, "internal_error", "unknown route", "api_error")
	}
}

func (s *Server) serveLocalCapable(w http.ResponseWriter, r *http.Request, decision policy.Decision, endpoint localCapableEndpoint) {
	forced := decision.Reason == policy.ReasonForced
	if s.local == nil {
		if s.tr != nil {
			s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonPolicy, endpoint.family)
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
			s.stats.burstsFull.Add(1)
			s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstFull, endpoint.family)
			return
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

	resp, err := endpoint.localPost(r.Context(), decision.LocalBody, r.Header)
	if err != nil {
		if !forced && s.cfg.BurstOnError && s.tr != nil {
			s.stats.burstsError.Add(1)
			releaseLocalSlot()
			s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstError, endpoint.family)
			return
		}
		writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", err.Error(), "api_error")
		return
	}
	defer resp.Body.Close()

	shouldBurst, err := shouldBurstLocalResponse(resp, decision.View.Model)
	if err != nil {
		writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", err.Error(), "api_error")
		return
	}
	if shouldBurst && !forced && s.cfg.BurstOnError && s.tr != nil {
		s.stats.burstsError.Add(1)
		closeBurstResponseBody(resp)
		releaseLocalSlot()
		s.serveTrustedRouterRaw(w, r, endpoint.trPath, decision.TRBody, decision.View.Stream, policy.ReasonBurstError, endpoint.family)
		return
	}
	if shouldBurst && forced {
		closeBurstResponseBody(resp)
		writeRoutedError(w, policy.RouteLocal, decision.Reason, http.StatusBadGateway, "local_upstream_error", fmt.Sprintf("local upstream returned status %d", resp.StatusCode), "api_error")
		return
	}

	s.serveUpstreamResponse(w, resp, policy.RouteLocal, decision.Reason, decision.View.Stream, endpoint.family)
}

func (s *Server) serveTrustedRouterRaw(w http.ResponseWriter, r *http.Request, path string, body []byte, requestStream bool, reason policy.Reason, endpoint endpointFamily) {
	if s.tr == nil {
		w.Header().Set("Retry-After", "1")
		writeRoutedError(w, policy.RouteLocal, reason, http.StatusTooManyRequests, "local_overloaded", "TrustedRouter is not configured", "rate_limit_error")
		return
	}
	resp, err := s.tr.RawPost(r.Context(), path, body, r.Header)
	if err != nil {
		status, message := trustedRouterError(err)
		writeRoutedError(w, policy.RouteTrustedRouter, reason, status, "trustedrouter_upstream_error", message, "api_error")
		return
	}
	defer resp.Body.Close()
	s.serveUpstreamResponse(w, resp, policy.RouteTrustedRouter, reason, requestStream, endpoint)
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
	s.serveTrustedRouterRaw(w, r, path, decision.TRBody, decision.View.Stream, decision.Reason, endpoint)
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

func (s *Server) serveUpstreamResponse(w http.ResponseWriter, resp *http.Response, route policy.Route, reason policy.Reason, requestStream bool, endpoint endpointFamily) {
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
	streamBody(w, resp)
}

func streamBody(w http.ResponseWriter, resp *http.Response) {
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, 32*1024)
	_, _ = io.CopyBuffer(flushWriter{w: w, flusher: flusher}, resp.Body, buf)
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

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	want := "Bearer " + s.cfg.Token
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		return true
	}
	writeRoutedError(w, s.defaultRoute(), policy.ReasonPolicy, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", "authentication_error")
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
	inFlightLocal   expvar.Int
	burstsFull      expvar.Int
	burstsError     expvar.Int
	forcedLocal     expvar.Int
	forcedTR        expvar.Int
	requestsTotal   expvar.Int
	catalogErrors   expvar.Int
	routes          routeStats
	chatRoutes      routeStats
	embeddingRoutes routeStats
	messageRoutes   routeStats
	responseRoutes  routeStats
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
		"in_flight_local": s.inFlightLocal.Value(),
		"bursts_full":     s.burstsFull.Value(),
		"bursts_error":    s.burstsError.Value(),
		"forced_local":    s.forcedLocal.Value(),
		"forced_tr":       s.forcedTR.Value(),
		"requests_total":  s.requestsTotal.Value(),
		"catalog_errors":  s.catalogErrors.Value(),
		"routes":          s.routes.snapshot(),
		"endpoint_routes": map[string]any{
			string(endpointChatCompletions): s.chatRoutes.snapshot(),
			string(endpointEmbeddings):      s.embeddingRoutes.snapshot(),
			string(endpointMessages):        s.messageRoutes.snapshot(),
			string(endpointResponses):       s.responseRoutes.snapshot(),
		},
	}
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
	list, err := s.tr.Models(ctx)
	if err == nil {
		return trustedModelsToMaps(list)
	}
	if !isTrustedRouterNotFound(err) {
		return nil, err
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
