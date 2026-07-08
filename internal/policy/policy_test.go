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

func TestDecideAliases(t *testing.T) {
	t.Parallel()

	aliases := map[string]string{
		"gpt-4o":       "qwen2.5-coder:32b",
		"local/gpt-4o": "should-not-win",
		"openai/gpt-4": "llama3.1",
	}

	t.Run("rewrites local body and preserves trustedrouter body", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"model":"gpt-4o","provider":{"sort":"price"},"messages":[]}`)
		got, err := Decide(raw, true, true, Options{Aliases: aliases})
		if err != nil {
			t.Fatalf("Decide() error = %v", err)
		}
		if got.Route != RouteLocal || got.Reason != ReasonPolicy {
			t.Fatalf("route/reason = %s/%s, want local/policy", got.Route, got.Reason)
		}
		assertJSONEqual(t, got.LocalBody, []byte(`{"model":"qwen2.5-coder:32b","messages":[]}`))
		if !bytes.Equal(got.TRBody, raw) {
			t.Fatalf("TRBody = %s, want original %s", got.TRBody, raw)
		}
		if !got.BurstAllowed || got.BurstSkippedUnmapped {
			t.Fatalf("burst flags allowed=%v skipped=%v, want true/false", got.BurstAllowed, got.BurstSkippedUnmapped)
		}
		if got.AliasKey != "gpt-4o" {
			t.Fatalf("AliasKey = %q, want gpt-4o", got.AliasKey)
		}
	})

	t.Run("local prefix wins over alias", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"model":"local/gpt-4o","messages":[]}`)
		got, err := Decide(raw, true, true, Options{Aliases: aliases})
		if err != nil {
			t.Fatalf("Decide() error = %v", err)
		}
		if got.Route != RouteLocal || got.Reason != ReasonForced {
			t.Fatalf("route/reason = %s/%s, want local/forced", got.Route, got.Reason)
		}
		assertJSONEqual(t, got.LocalBody, []byte(`{"model":"gpt-4o","messages":[]}`))
		if got.BurstAllowed || got.BurstSkippedUnmapped {
			t.Fatalf("burst flags allowed=%v skipped=%v, want false/false", got.BurstAllowed, got.BurstSkippedUnmapped)
		}
		if got.AliasKey != "" {
			t.Fatalf("AliasKey = %q, want empty", got.AliasKey)
		}
	})
}

func TestDecideNormalizationEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       []byte
		route     Route
		reason    Reason
		localBody []byte
	}{
		{
			name:      "provider only mixed-case Local",
			raw:       []byte(`{"model":"x","provider":{"only":["Local"]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"x","messages":[]}`),
		},
		{
			name:      "provider only padded local",
			raw:       []byte(`{"model":"x","provider":{"only":[" local "]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"x","messages":[]}`),
		},
		{
			name:      "provider only comma-string local",
			raw:       []byte(`{"model":"x","provider":{"only":"local"},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"x","messages":[]}`),
		},
		{
			name:      "provider only local with junk empty entry stays a local pin",
			raw:       []byte(`{"model":"anthropic/claude","provider":{"only":["local",""]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"anthropic/claude","messages":[]}`),
		},
		{
			name:      "provider only all-empty is not a local pin",
			raw:       []byte(`{"model":"llama3","provider":{"only":["",""]},"messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonPolicy,
			localBody: []byte(`{"model":"llama3","messages":[]}`),
		},
		{
			name:   "provider order comma-string names external",
			raw:    []byte(`{"model":"trustedrouter/auto","provider":{"order":"local,anthropic"},"messages":[]}`),
			route:  RouteTrustedRouter,
			reason: ReasonForced,
		},
		{
			name:      "bare-string provider local",
			raw:       []byte(`{"model":"x","provider":"local","messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"x","messages":[]}`),
		},
		{
			name:   "bare-string provider external",
			raw:    []byte(`{"model":"x","provider":"anthropic","messages":[]}`),
			route:  RouteTrustedRouter,
			reason: ReasonForced,
		},
		{
			name:      "local prefix mixed-case strips and preserves suffix",
			raw:       []byte(`{"model":"Local/Llama3","messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"Llama3","messages":[]}`),
		},
		{
			name:      "local prefix padded strips and preserves suffix",
			raw:       []byte(`{"model":" local/Llama3 ","messages":[]}`),
			route:     RouteLocal,
			reason:    ReasonForced,
			localBody: []byte(`{"model":"Llama3","messages":[]}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decide(tt.raw, true, true)
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
				return
			}
			assertJSONEqual(t, got.LocalBody, tt.localBody)
		})
	}
}

func TestDecideFoldsMaxTokensAliasesIntoLocalBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       []byte
		localBody []byte
	}{
		{
			name:      "max_completion_tokens folded",
			raw:       []byte(`{"model":"local/qwen","max_completion_tokens":256,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_completion_tokens":256,"messages":[],"max_tokens":256}`),
		},
		{
			name:      "max_output_tokens folded",
			raw:       []byte(`{"model":"local/qwen","max_output_tokens":128,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_output_tokens":128,"messages":[],"max_tokens":128}`),
		},
		{
			name:      "explicit max_tokens wins",
			raw:       []byte(`{"model":"local/qwen","max_tokens":64,"max_completion_tokens":256,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_tokens":64,"max_completion_tokens":256,"messages":[]}`),
		},
		{
			name:      "non-numeric alias is not folded",
			raw:       []byte(`{"model":"local/qwen","max_completion_tokens":null,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_completion_tokens":null,"messages":[]}`),
		},
		{
			name:      "null max_tokens does not block the fold",
			raw:       []byte(`{"model":"local/qwen","max_tokens":null,"max_completion_tokens":256,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_completion_tokens":256,"messages":[],"max_tokens":256}`),
		},
		{
			name:      "float alias is not folded",
			raw:       []byte(`{"model":"local/qwen","max_completion_tokens":256.5,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_completion_tokens":256.5,"messages":[]}`),
		},
		{
			name:      "exponent alias is not folded",
			raw:       []byte(`{"model":"local/qwen","max_output_tokens":1e5,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_output_tokens":1e5,"messages":[]}`),
		},
		{
			name:      "overflow alias is not folded",
			raw:       []byte(`{"model":"local/qwen","max_completion_tokens":99999999999999999999,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_completion_tokens":99999999999999999999,"messages":[]}`),
		},
		{
			name:      "zero alias is not folded",
			raw:       []byte(`{"model":"local/qwen","max_completion_tokens":0,"messages":[]}`),
			localBody: []byte(`{"model":"qwen","max_completion_tokens":0,"messages":[]}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decide(tt.raw, true, true)
			if err != nil {
				t.Fatalf("Decide() error = %v", err)
			}
			assertJSONEqual(t, got.LocalBody, tt.localBody)
		})
	}
}

func TestDecideRejectsNonCanonicalTopLevelKeys(t *testing.T) {
	t.Parallel()

	// encoding/json matches fields case-insensitively; the raw splice is
	// exact-case. A non-canonical spelling of a key Bursty reads/rewrites would
	// let the routing decision and the body rewrite disagree, so it is rejected.
	for _, raw := range [][]byte{
		[]byte(`{"model":"openai/gpt-4o","Model":"local/qwen","messages":[]}`),
		[]byte(`{"Model":"local/qwen","messages":[]}`),
		[]byte(`{"model":"llama3","Stream":true,"messages":[]}`),
		[]byte(`{"model":"llama3","Max_Tokens":10,"messages":[]}`),
	} {
		_, err := Decide(raw, true, true)
		if err == nil || !strings.Contains(err.Error(), "non-canonical top-level key") {
			t.Fatalf("Decide(%s) error = %v, want non-canonical top-level key", raw, err)
		}
	}
}

func TestDecideRejectsEmptyLocalPrefix(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{
		[]byte(`{"model":"local/","messages":[]}`),
		[]byte(`{"model":"local/  ","messages":[]}`),
		[]byte(`{"model":" LOCAL/ ","messages":[]}`),
	} {
		_, err := Decide(raw, true, true)
		var configErr *ConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("Decide(%s) error = %v, want ConfigError", raw, err)
		}
		if configErr.Code != "invalid_local_model" {
			t.Fatalf("ConfigError code = %q, want invalid_local_model", configErr.Code)
		}
	}
}

func TestDecideAliasLookupCaseInsensitive(t *testing.T) {
	t.Parallel()

	// Alias keys are stored lowercased by config parsing; a mixed-case request
	// model must still resolve to the local target instead of bursting to cloud.
	aliases := map[string]string{"claude-haiku-4.5": "qwen2.5-coder:7b"}
	raw := []byte(`{"model":"Claude-Haiku-4.5","messages":[]}`)
	got, err := Decide(raw, true, true, Options{Aliases: aliases})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if got.Route != RouteLocal || got.Reason != ReasonPolicy {
		t.Fatalf("route/reason = %s/%s, want local/policy", got.Route, got.Reason)
	}
	assertJSONEqual(t, got.LocalBody, []byte(`{"model":"qwen2.5-coder:7b","messages":[]}`))
	if !got.BurstAllowed {
		t.Fatalf("BurstAllowed = false, want true for an aliased request")
	}
}

func TestDecideInjectsStreamUsageOnlyIntoLocalBody(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"gpt-4o","stream":true,"messages":[]}`)
	got, err := Decide(raw, true, true, Options{Aliases: map[string]string{"gpt-4o": "llama3"}})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	assertJSONEqual(t, got.LocalBody, []byte(`{"model":"llama3","stream":true,"messages":[],"stream_options":{"include_usage":true}}`))
	if !bytes.Equal(got.TRBody, raw) {
		t.Fatalf("TRBody = %s, want original %s", got.TRBody, raw)
	}
}

func TestDecideUnmappedLocalModelBurstPolicy(t *testing.T) {
	t.Parallel()

	t.Run("unmapped local-native id suppresses burst", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"model":"llama3.2","messages":[]}`)
		got, err := Decide(raw, true, true)
		if err != nil {
			t.Fatalf("Decide() error = %v", err)
		}
		if got.Route != RouteLocal || got.Reason != ReasonPolicy {
			t.Fatalf("route/reason = %s/%s, want local/policy", got.Route, got.Reason)
		}
		if got.BurstAllowed || !got.BurstSkippedUnmapped {
			t.Fatalf("burst flags allowed=%v skipped=%v, want false/true", got.BurstAllowed, got.BurstSkippedUnmapped)
		}
		if !bytes.Equal(got.TRBody, raw) {
			t.Fatalf("TRBody = %s, want original %s", got.TRBody, raw)
		}
	})

	t.Run("fallback model substitutes trustedrouter body", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"model":"llama3.2","messages":[]}`)
		got, err := Decide(raw, true, true, Options{BurstFallbackModel: "openai/gpt-4o-mini"})
		if err != nil {
			t.Fatalf("Decide() error = %v", err)
		}
		if !got.BurstAllowed || got.BurstSkippedUnmapped {
			t.Fatalf("burst flags allowed=%v skipped=%v, want true/false", got.BurstAllowed, got.BurstSkippedUnmapped)
		}
		assertJSONEqual(t, got.LocalBody, raw)
		assertJSONEqual(t, got.TRBody, []byte(`{"model":"openai/gpt-4o-mini","messages":[]}`))
	})

	t.Run("namespaced model can burst without fallback", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"model":"openai/gpt-4o","messages":[]}`)
		got, err := Decide(raw, true, true)
		if err != nil {
			t.Fatalf("Decide() error = %v", err)
		}
		if !got.BurstAllowed || got.BurstSkippedUnmapped {
			t.Fatalf("burst flags allowed=%v skipped=%v, want true/false", got.BurstAllowed, got.BurstSkippedUnmapped)
		}
		if !bytes.Equal(got.TRBody, raw) {
			t.Fatalf("TRBody = %s, want original %s", got.TRBody, raw)
		}
	})
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

func TestDecideRejectsDuplicateTopLevelKeys(t *testing.T) {
	t.Parallel()

	// Duplicate occurrences of any key BurstyRouter reads or rewrites are
	// ambiguous (last-wins decoders vs first-occurrence splicing), so they are
	// refused rather than folded/rewritten into a corrupted body.
	for _, raw := range [][]byte{
		[]byte(`{"model":"llama3","model":"mistral","messages":[]}`),
		[]byte(`{"model":"llama3","provider":{"order":["local"]},"provider":{"order":["anthropic"]},"messages":[]}`),
		[]byte(`{"model":"local/qwen","max_tokens":null,"max_completion_tokens":256,"max_tokens":999,"messages":[]}`),
		[]byte(`{"model":"local/qwen","max_completion_tokens":128,"max_completion_tokens":256,"messages":[]}`),
		[]byte(`{"model":"local/qwen","max_output_tokens":128,"max_output_tokens":256,"messages":[]}`),
		[]byte(`{"model":"llama3","stream":false,"stream":true,"messages":[]}`),
		[]byte(`{"model":"llama3","stream":true,"stream_options":{"include_usage":false},"stream_options":{"include_usage":true},"messages":[]}`),
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
