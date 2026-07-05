package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecideDirectiveMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       []byte
		route     Route
		reason    Reason
		localBody []byte
		hasLocal  bool
		hasTR     bool
	}{
		{
			name:      "provider only local",
			raw:       []byte(`{"model":"llama3","provider":{"only":["local"]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
			hasLocal:  true,
			hasTR:     true,
		},
		{
			name:      "provider only all local",
			raw:       []byte(`{"model":"llama3","provider":{"only":["local","local"]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
			hasLocal:  true,
			hasTR:     true,
		},
		{
			name:      "provider order local is preference",
			raw:       []byte(`{"model":"llama3","provider":{"order":["local"]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonPolicy,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
			hasLocal:  true,
			hasTR:     true,
		},
		{
			name:     "provider order external",
			raw:      []byte(`{"model":"trustedrouter/auto","provider":{"order":["anthropic"]},"messages":[]}`),
			route:    RouteTrustedRouter,
			reason:   ReasonForced,
			hasLocal: true,
			hasTR:    true,
		},
		{
			name:      "local model prefix",
			raw:       []byte(`{"model":"local/llama3","provider":{"order":["local"]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
			hasLocal:  true,
			hasTR:     true,
		},
		{
			name:      "default local first",
			raw:       []byte(`{"model":"llama3","messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonPolicy,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
			hasLocal:  true,
			hasTR:     true,
		},
		{
			name:      "default local strips provider shaping args",
			raw:       []byte(`{"model":"llama3","provider":{"sort":"price"},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonPolicy,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
			hasLocal:  true,
			hasTR:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decide(tt.raw, tt.hasLocal, tt.hasTR)
			if err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			if got.Route != tt.route || got.Reason != tt.reason {
				t.Fatalf("route/reason = %s/%s, want %s/%s", got.Route, got.Reason, tt.route, tt.reason)
			}
			if !bytes.Equal(got.TRBody, tt.raw) {
				t.Fatalf("TRBody = %s, want verbatim %s", got.TRBody, tt.raw)
			}
			if tt.localBody == nil {
				if got.LocalBody != nil {
					t.Fatalf("LocalBody = %s, want nil", got.LocalBody)
				}
				return
			}
			if !bytes.Equal(got.LocalBody, tt.localBody) {
				t.Fatalf("LocalBody = %s, want %s", got.LocalBody, tt.localBody)
			}
		})
	}
}

func TestDecideFailClosedMissingUpstreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      []byte
		hasLocal bool
		hasTR    bool
		route    Route
		message  string
	}{
		{
			name:    "local only without local",
			raw:     []byte(`{"model":"llama3","provider":{"only":["local"]},"messages":[]}`),
			hasTR:   true,
			route:   RouteLocal,
			message: "local upstream is not configured; request is pinned to local",
		},
		{
			name:    "local model prefix without local",
			raw:     []byte(`{"model":"local/llama3","messages":[]}`),
			hasTR:   true,
			route:   RouteLocal,
			message: "local upstream is not configured; request is pinned to local",
		},
		{
			name:     "provider only external without trustedrouter",
			raw:      []byte(`{"model":"llama3","provider":{"only":["anthropic"]},"messages":[]}`),
			hasLocal: true,
			route:    RouteTrustedRouter,
			message:  "TrustedRouter is not configured; request requires providers [anthropic]",
		},
		{
			name:     "provider order external without trustedrouter",
			raw:      []byte(`{"model":"llama3","provider":{"order":["openai"]},"messages":[]}`),
			hasLocal: true,
			route:    RouteTrustedRouter,
			message:  "TrustedRouter is not configured; request requires providers [openai]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decide(tt.raw, tt.hasLocal, tt.hasTR)
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("Decide() error = %v, want ConfigError", err)
			}
			if configErr.Route != tt.route || configErr.Message != tt.message {
				t.Fatalf("ConfigError = %#v, want route %s message %q", configErr, tt.route, tt.message)
			}
		})
	}
}

func TestDecideTrustedRouterOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    []byte
		route  Route
		reason Reason
	}{
		{
			name:   "default trustedrouter",
			raw:    []byte(`{"model":"x","messages":[]}`),
			route:  RouteTrustedRouter,
			reason: ReasonPolicy,
		},
		{
			name:   "provider external forced",
			raw:    []byte(`{"model":"x","provider":{"order":["anthropic"]},"messages":[]}`),
			route:  RouteTrustedRouter,
			reason: ReasonForced,
		},
		{
			name:   "provider only local unsupported",
			raw:    []byte(`{"model":"x","provider":{"only":["local"]},"messages":[]}`),
			route:  RouteLocal,
			reason: ReasonForced,
		},
		{
			name:   "local model prefix unsupported",
			raw:    []byte(`{"model":"local/x","messages":[]}`),
			route:  RouteLocal,
			reason: ReasonForced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecideTrustedRouterOnly(tt.raw)
			if err != nil {
				t.Fatalf("DecideTrustedRouterOnly() error = %v", err)
			}
			if got.Route != tt.route || got.Reason != tt.reason {
				t.Fatalf("route/reason = %s/%s, want %s/%s", got.Route, got.Reason, tt.route, tt.reason)
			}
			if !bytes.Equal(got.TRBody, tt.raw) {
				t.Fatalf("TRBody = %s, want verbatim %s", got.TRBody, tt.raw)
			}
		})
	}
}

func TestDecideRejectsDuplicateRoutingKeys(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{
		[]byte(`{"model":"llama3","model":"mistral","messages":[]}`),
		[]byte(`{"model":"llama3","provider":{"order":["local"]},"provider":{"order":["anthropic"]},"messages":[]}`),
	} {
		_, err := Decide(raw, true, true)
		if err == nil || !strings.Contains(err.Error(), "duplicate top-level key") {
			t.Fatalf("Decide(%s) error = %v, want duplicate top-level key", raw, err)
		}
	}
}

func TestRawSpliceHelpers(t *testing.T) {
	t.Parallel()

	removeTests := []struct {
		name string
		raw  string
		want string
	}{
		{"first", `{"provider":{"only":["local"]},"model":"x","messages":[]}`, `{"model":"x","messages":[]}`},
		{"middle", `{"model":"x","provider":{"only":["local"]},"messages":[]}`, `{"model":"x","messages":[]}`},
		{"last", `{"model":"x","messages":[],"provider":{"only":["local"]}}`, `{"model":"x","messages":[]}`},
		{"single", `{"provider":{"only":["local"]}}`, `{}`},
	}
	for _, tt := range removeTests {
		t.Run("remove "+tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := RemoveTopLevelKey([]byte(tt.raw), "provider")
			if err != nil {
				t.Fatalf("RemoveTopLevelKey() error = %v", err)
			}
			assertJSONEqual(t, got, []byte(tt.want))
		})
	}

	t.Run("nested provider untouched", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"model":"x","metadata":{"provider":true},"messages":[{"role":"user","content":"{\"provider\":1}"}],"provider":{"only":["local"]}}`)
		got, err := RemoveTopLevelKey(raw, "provider")
		if err != nil {
			t.Fatalf("RemoveTopLevelKey() error = %v", err)
		}
		var obj map[string]any
		if err := json.Unmarshal(got, &obj); err != nil {
			t.Fatalf("result is invalid JSON: %v\n%s", err, got)
		}
		if _, ok := obj["provider"]; ok {
			t.Fatalf("top-level provider still present: %s", got)
		}
		metadata := obj["metadata"].(map[string]any)
		if metadata["provider"] != true {
			t.Fatalf("nested provider was changed: %#v", metadata)
		}
	})

	t.Run("replace escaped model string", func(t *testing.T) {
		t.Parallel()
		got, err := ReplaceTopLevelString([]byte(`{"model":"local/llama\"3","messages":[]}`), "model", `llama"3`)
		if err != nil {
			t.Fatalf("ReplaceTopLevelString() error = %v", err)
		}
		assertJSONEqual(t, got, []byte(`{"model":"llama\"3","messages":[]}`))
	})

	t.Run("inject bursty object", func(t *testing.T) {
		t.Parallel()
		got, err := InjectTopLevelObject([]byte(`{"id":"abc"}`), "bursty", []byte(`{"route":"local","reason":"policy"}`))
		if err != nil {
			t.Fatalf("InjectTopLevelObject() error = %v", err)
		}
		assertJSONEqual(t, got, []byte(`{"id":"abc","bursty":{"route":"local","reason":"policy"}}`))
	})
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotAny any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("got invalid JSON: %v\n%s", err, got)
	}
	var wantAny any
	if err := json.Unmarshal(want, &wantAny); err != nil {
		t.Fatalf("want invalid JSON: %v\n%s", err, want)
	}
	gotCanonical, _ := json.Marshal(gotAny)
	wantCanonical, _ := json.Marshal(wantAny)
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("JSON = %s, want %s", gotCanonical, wantCanonical)
	}
}
