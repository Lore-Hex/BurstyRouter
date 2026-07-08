package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Route is the selected upstream route.
type Route string

const (
	RouteLocal         Route = "local"
	RouteTrustedRouter Route = "trustedrouter"
)

// Reason explains why a route was selected.
type Reason string

const (
	ReasonForced     Reason = "forced"
	ReasonPolicy     Reason = "policy"
	ReasonBurstFull  Reason = "burst-full"
	ReasonBurstError Reason = "burst-error"
	ReasonBurstSlow  Reason = "burst-slow"
)

// ProviderDirective is the minimal provider routing shape BurstyRouter needs.
type ProviderDirective struct {
	Only  stringList `json:"only"`
	Order stringList `json:"order"`
}

// UnmarshalJSON tolerates a bare-string provider (e.g. "provider":"local") by
// treating it as a single-entry restriction, so those requests route instead
// of failing to decode. Objects decode normally.
func (p *ProviderDirective) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var single string
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return err
		}
		if value := strings.TrimSpace(single); value != "" {
			p.Only = stringList{value}
		}
		return nil
	}
	type alias ProviderDirective
	var tmp alias
	if err := json.Unmarshal(trimmed, &tmp); err != nil {
		return err
	}
	*p = ProviderDirective(tmp)
	return nil
}

// stringList decodes from either a JSON array of strings or a single
// comma-separated string (OpenRouter's comma form), mirroring the enclave's
// lenient provider-list handling so both shapes route the same way.
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = nil
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*s = arr
		return nil
	}
	var single string
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return err
	}
	parts := strings.Split(single, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	*s = out
	return nil
}

// localEntry reports whether a provider entry names the local upstream,
// tolerant of case and surrounding whitespace.
func localEntry(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "local")
}

// localModelTarget reports whether a model id is pinned to local via the
// "local/" prefix (case-insensitive, whitespace-tolerant) and returns the
// downstream model name with the prefix stripped. The suffix case is preserved
// because local model ids are case-sensitive.
func localModelTarget(model string) (string, bool) {
	trimmed := strings.TrimSpace(model)
	const prefix = "local/"
	if len(trimmed) < len(prefix) || !strings.EqualFold(trimmed[:len(prefix)], prefix) {
		return "", false
	}
	return trimmed[len(prefix):], true
}

// RequestView is a minimal decoded view of a chat request. Raw forwarding
// paths keep the original request bytes and do not re-encode this structure.
type RequestView struct {
	Model    string             `json:"model"`
	Stream   bool               `json:"stream"`
	Provider *ProviderDirective `json:"provider"`
}

// Options controls policy-only request rewriting and burst eligibility.
type Options struct {
	Aliases            map[string]string
	BurstFallbackModel string
}

// Decision is a policy decision plus the body bytes to forward.
type Decision struct {
	Route                Route
	Reason               Reason
	LocalBody            []byte
	TRBody               []byte
	View                 RequestView
	AliasKey             string
	BurstAllowed         bool
	BurstSkippedUnmapped bool
}

// ConfigError is returned when the request explicitly requires an upstream
// that is not configured. Callers should surface it as a Bursty-origin routing
// error, not as a JSON decode failure.
type ConfigError struct {
	Route   Route
	Code    string
	Message string
	Type    string
}

func (e *ConfigError) Error() string {
	return e.Message
}

// Decide selects an upstream and applies the raw local-only body rewrites.
func Decide(raw []byte, hasLocal, hasTrustedRouter bool, options ...Options) (Decision, error) {
	view, err := DecodeRequestView(raw)
	if err != nil {
		return Decision{}, err
	}
	opts := firstOptions(options)
	aliasTarget, aliased := aliasFor(view, opts.Aliases)

	decision := Decision{TRBody: raw, View: view}
	if aliased {
		decision.AliasKey = view.Model
	}
	localBody := func() error {
		if decision.LocalBody != nil {
			return nil
		}
		body, err := localForwardBody(raw, view, aliasTarget)
		if err != nil {
			return err
		}
		decision.LocalBody = body
		return nil
	}
	local := func(reason Reason) (Decision, error) {
		if err := localBody(); err != nil {
			return Decision{}, err
		}
		decision.Route = RouteLocal
		decision.Reason = reason
		if reason == ReasonPolicy && hasTrustedRouter {
			if err := applyBurstPolicy(&decision, raw, view, aliased, opts.BurstFallbackModel); err != nil {
				return Decision{}, err
			}
		}
		return decision, nil
	}
	trustedRouter := func(reason Reason) (Decision, error) {
		decision.Route = RouteTrustedRouter
		decision.Reason = reason
		return decision, nil
	}

	if !hasLocal && localPinned(view) {
		return Decision{}, &ConfigError{
			Route:   RouteLocal,
			Code:    "no_local_upstream",
			Message: "local upstream is not configured; request is pinned to local",
			Type:    "api_error",
		}
	}
	if !hasTrustedRouter {
		required := requiredNonLocalProviders(view.Provider)
		if len(required) > 0 {
			return Decision{}, &ConfigError{
				Route:   RouteTrustedRouter,
				Code:    "no_trustedrouter_upstream",
				Message: fmt.Sprintf("TrustedRouter is not configured; request requires providers %v", required),
				Type:    "api_error",
			}
		}
	}

	if mentionsNonLocal(view.Provider) {
		return trustedRouter(ReasonForced)
	}
	if !hasLocal {
		return trustedRouter(ReasonPolicy)
	}
	if !hasTrustedRouter {
		if localPinned(view) {
			return local(ReasonForced)
		}
		return local(ReasonPolicy)
	}

	if isLocalOnly(view.Provider) {
		return local(ReasonForced)
	}
	if _, ok := localModelTarget(view.Model); ok {
		return local(ReasonForced)
	}
	return local(ReasonPolicy)
}

// DecideTrustedRouterOnly parses a request for endpoints local OpenAI servers
// cannot serve. It preserves the original body for TrustedRouter forwarding.
func DecideTrustedRouterOnly(raw []byte) (Decision, error) {
	view, err := DecodeRequestView(raw)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		Route:  RouteTrustedRouter,
		Reason: ReasonPolicy,
		TRBody: raw,
		View:   view,
	}
	if localPinned(view) {
		decision.Route = RouteLocal
		decision.Reason = ReasonForced
		return decision, nil
	}
	if mentionsNonLocal(view.Provider) {
		decision.Reason = ReasonForced
	}
	return decision, nil
}

// DecodeRequestView validates the top-level routing object and decodes the
// fields needed by BurstyRouter policy.
func DecodeRequestView(raw []byte) (RequestView, error) {
	if _, err := scanTopLevelObject(raw); err != nil {
		return RequestView{}, err
	}
	var view RequestView
	if err := json.Unmarshal(raw, &view); err != nil {
		return RequestView{}, fmt.Errorf("decode request body: %w", err)
	}
	return view, nil
}

func localForwardBody(raw []byte, view RequestView, aliasTarget string) ([]byte, error) {
	body, err := RemoveTopLevelKey(raw, "provider")
	if err != nil {
		return nil, err
	}
	if target, ok := localModelTarget(view.Model); ok {
		if strings.TrimSpace(target) == "" {
			return nil, &ConfigError{
				Route:   RouteLocal,
				Code:    "invalid_local_model",
				Message: `model "local/" requires a model name after the prefix`,
				Type:    "invalid_request_error",
			}
		}
		body, err = ReplaceTopLevelString(body, "model", target)
		if err != nil {
			return nil, err
		}
	} else if aliasTarget != "" {
		body, err = ReplaceTopLevelString(body, "model", aliasTarget)
		if err != nil {
			return nil, err
		}
	}
	if view.Stream {
		body, err = injectStreamUsage(body)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func injectStreamUsage(raw []byte) ([]byte, error) {
	payload := []byte(`{"include_usage":true}`)
	return InjectTopLevelObject(raw, "stream_options", payload)
}

func firstOptions(options []Options) Options {
	if len(options) == 0 {
		return Options{}
	}
	return options[0]
}

func aliasFor(view RequestView, aliases map[string]string) (string, bool) {
	if _, isLocal := localModelTarget(view.Model); isLocal || len(aliases) == 0 {
		return "", false
	}
	target, ok := aliases[strings.ToLower(strings.TrimSpace(view.Model))]
	return target, ok
}

func applyBurstPolicy(decision *Decision, raw []byte, view RequestView, aliased bool, fallbackModel string) error {
	if aliased || cloudRoutableModel(view.Model) {
		decision.BurstAllowed = true
		return nil
	}
	fallbackModel = strings.TrimSpace(fallbackModel)
	if fallbackModel == "" {
		decision.BurstSkippedUnmapped = true
		return nil
	}
	body, err := ReplaceTopLevelString(raw, "model", fallbackModel)
	if err != nil {
		return err
	}
	decision.TRBody = body
	decision.BurstAllowed = true
	return nil
}

func cloudRoutableModel(model string) bool {
	if _, ok := localModelTarget(model); ok {
		return false
	}
	trimmed := strings.TrimSpace(model)
	return strings.Contains(trimmed, "/") || strings.HasPrefix(strings.ToLower(trimmed), "trustedrouter/")
}

func isLocalOnly(provider *ProviderDirective) bool {
	if provider == nil || len(provider.Only) == 0 {
		return false
	}
	for _, value := range provider.Only {
		if !localEntry(value) {
			return false
		}
	}
	return true
}

func localPinned(view RequestView) bool {
	if isLocalOnly(view.Provider) {
		return true
	}
	_, ok := localModelTarget(view.Model)
	return ok
}

func mentionsNonLocal(provider *ProviderDirective) bool {
	return len(requiredNonLocalProviders(provider)) > 0
}

func requiredNonLocalProviders(provider *ProviderDirective) []string {
	if provider == nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(value string) {
		if localEntry(value) {
			return
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, value := range provider.Only {
		add(value)
	}
	// Order is a preference, not a restriction: ["local"] does not force local.
	for _, value := range provider.Order {
		add(value)
	}
	return out
}
