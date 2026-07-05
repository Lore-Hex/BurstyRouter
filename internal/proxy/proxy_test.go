package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lore-Hex/BurstyRouter/internal/config"
	"github.com/Lore-Hex/BurstyRouter/internal/upstream"
)

func TestChatDirectiveMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantRoute  string
		wantReason string
		wantLocal  bool
		wantTR     bool
		wantBody   string
		verbatim   bool
	}{
		{
			name:       "provider only local",
			body:       `{"model":"llama3","provider":{"only":["local"]},"messages":[]}`,
			wantRoute:  "local",
			wantReason: "forced",
			wantLocal:  true,
			wantBody:   `{"model":"llama3","messages":[]}`,
		},
		{
			name:       "provider only all local",
			body:       `{"model":"llama3","provider":{"only":["local","local"]},"messages":[]}`,
			wantRoute:  "local",
			wantReason: "forced",
			wantLocal:  true,
			wantBody:   `{"model":"llama3","messages":[]}`,
		},
		{
			name:       "provider order local is preference",
			body:       `{"model":"llama3","provider":{"order":["local"]},"messages":[]}`,
			wantRoute:  "local",
			wantReason: "policy",
			wantLocal:  true,
			wantBody:   `{"model":"llama3","messages":[]}`,
		},
		{
			name:       "provider order external",
			body:       `{"model":"trustedrouter/auto","provider":{"order":["anthropic"]},"messages":[]}`,
			wantRoute:  "trustedrouter",
			wantReason: "forced",
			wantTR:     true,
			verbatim:   true,
		},
		{
			name:       "local model prefix",
			body:       `{"model":"local/llama3","provider":{"order":["local"]},"messages":[]}`,
			wantRoute:  "local",
			wantReason: "forced",
			wantLocal:  true,
			wantBody:   `{"model":"llama3","messages":[]}`,
		},
		{
			name:       "default local first",
			body:       `{"model":"llama3","messages":[]}`,
			wantRoute:  "local",
			wantReason: "policy",
			wantLocal:  true,
			verbatim:   true,
		},
		{
			name:       "default local strips provider shaping args",
			body:       `{"model":"llama3","provider":{"sort":"price"},"messages":[]}`,
			wantRoute:  "local",
			wantReason: "policy",
			wantLocal:  true,
			wantBody:   `{"model":"llama3","messages":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var localBody, trBody []byte
			var bodyMu sync.Mutex
			var localCalls, trCalls atomic.Int64

			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("local path = %s", r.URL.Path)
				}
				localCalls.Add(1)
				body, _ := io.ReadAll(r.Body)
				bodyMu.Lock()
				localBody = body
				bodyMu.Unlock()
				writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
			})
			tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("tr path = %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer tr-key" {
					t.Errorf("tr auth = %q", got)
				}
				trCalls.Add(1)
				body, _ := io.ReadAll(r.Body)
				bodyMu.Lock()
				trBody = body
				bodyMu.Unlock()
				writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
			})
			proxy := newProxyWithHandlers(t, config.Config{
				TRAPIKey:     "tr-key",
				BurstOnError: true,
			}, local, tr)

			resp, body := postChat(t, proxy, tt.body, "")
			if resp.Header.Get("X-Bursty-Route") != tt.wantRoute || resp.Header.Get("X-Bursty-Reason") != tt.wantReason {
				t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
			}
			assertBurstyBlock(t, body, tt.wantRoute, tt.wantReason)

			if tt.wantLocal && localCalls.Load() != 1 {
				t.Fatalf("local calls = %d, want 1", localCalls.Load())
			}
			if !tt.wantLocal && localCalls.Load() != 0 {
				t.Fatalf("local calls = %d, want 0", localCalls.Load())
			}
			if tt.wantTR && trCalls.Load() != 1 {
				t.Fatalf("tr calls = %d, want 1", trCalls.Load())
			}
			if !tt.wantTR && trCalls.Load() != 0 {
				t.Fatalf("tr calls = %d, want 0", trCalls.Load())
			}

			bodyMu.Lock()
			gotBody := append([]byte(nil), localBody...)
			if tt.wantTR {
				gotBody = append([]byte(nil), trBody...)
			}
			bodyMu.Unlock()
			wantBody := []byte(tt.wantBody)
			if tt.verbatim {
				wantBody = []byte(tt.body)
				if !bytes.Equal(gotBody, wantBody) {
					t.Fatalf("forwarded body = %s, want verbatim %s", gotBody, wantBody)
				}
			} else {
				assertJSONEqual(t, gotBody, wantBody)
			}
		})
	}
}

func TestEmbeddingsDirectiveMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantRoute  string
		wantReason string
		wantLocal  bool
		wantTR     bool
		wantBody   string
		verbatim   bool
	}{
		{
			name:       "provider only local",
			body:       `{"model":"nomic-embed","provider":{"only":["local"]},"input":"hello"}`,
			wantRoute:  "local",
			wantReason: "forced",
			wantLocal:  true,
			wantBody:   `{"model":"nomic-embed","input":"hello"}`,
		},
		{
			name:       "local model prefix",
			body:       `{"model":"local/nomic-embed","input":"hello"}`,
			wantRoute:  "local",
			wantReason: "forced",
			wantLocal:  true,
			wantBody:   `{"model":"nomic-embed","input":"hello"}`,
		},
		{
			name:       "provider order external",
			body:       `{"model":"trustedrouter/auto","provider":{"order":["openai"]},"input":"hello"}`,
			wantRoute:  "trustedrouter",
			wantReason: "forced",
			wantTR:     true,
			verbatim:   true,
		},
		{
			name:       "default local first",
			body:       `{"model":"nomic-embed","input":"hello"}`,
			wantRoute:  "local",
			wantReason: "policy",
			wantLocal:  true,
			verbatim:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var localBody, trBody []byte
			var bodyMu sync.Mutex
			var localCalls, trCalls atomic.Int64

			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/embeddings" {
					t.Errorf("local path = %s", r.URL.Path)
				}
				localCalls.Add(1)
				body, _ := io.ReadAll(r.Body)
				bodyMu.Lock()
				localBody = body
				bodyMu.Unlock()
				writeTestJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
			})
			tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/embeddings" {
					t.Errorf("tr path = %s", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer tr-key" {
					t.Errorf("tr auth = %q", got)
				}
				trCalls.Add(1)
				body, _ := io.ReadAll(r.Body)
				bodyMu.Lock()
				trBody = body
				bodyMu.Unlock()
				writeTestJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
			})
			proxy := newProxyWithHandlers(t, config.Config{
				TRAPIKey:     "tr-key",
				BurstOnError: true,
			}, local, tr)

			resp, body := postJSON(t, proxy, embeddingsPath, tt.body, "")
			if resp.Header.Get("X-Bursty-Route") != tt.wantRoute || resp.Header.Get("X-Bursty-Reason") != tt.wantReason {
				t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
			}
			assertBurstyBlock(t, body, tt.wantRoute, tt.wantReason)

			if tt.wantLocal && localCalls.Load() != 1 {
				t.Fatalf("local calls = %d, want 1", localCalls.Load())
			}
			if !tt.wantLocal && localCalls.Load() != 0 {
				t.Fatalf("local calls = %d, want 0", localCalls.Load())
			}
			if tt.wantTR && trCalls.Load() != 1 {
				t.Fatalf("tr calls = %d, want 1", trCalls.Load())
			}
			if !tt.wantTR && trCalls.Load() != 0 {
				t.Fatalf("tr calls = %d, want 0", trCalls.Load())
			}

			bodyMu.Lock()
			gotBody := append([]byte(nil), localBody...)
			if tt.wantTR {
				gotBody = append([]byte(nil), trBody...)
			}
			bodyMu.Unlock()
			wantBody := []byte(tt.wantBody)
			if tt.verbatim {
				wantBody = []byte(tt.body)
				if !bytes.Equal(gotBody, wantBody) {
					t.Fatalf("forwarded body = %s, want verbatim %s", gotBody, wantBody)
				}
			} else {
				assertJSONEqual(t, gotBody, wantBody)
			}
		})
	}
}

func TestTrustedRouterOnlyDirectiveMatrix(t *testing.T) {
	t.Parallel()

	endpoints := map[string]string{
		messagesPath:  `{"model":"anthropic/claude-haiku-4.5","messages":[]}`,
		responsesPath: `{"model":"openai/gpt-4.1-mini","input":"hello"}`,
	}
	for endpoint, defaultBody := range endpoints {
		t.Run(endpoint+" default trustedrouter", func(t *testing.T) {
			t.Parallel()
			seenBody := make(chan []byte, 1)
			var calls atomic.Int64
			tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != endpoint {
					t.Fatalf("tr path = %s, want %s", r.URL.Path, endpoint)
				}
				calls.Add(1)
				gotBody, _ := io.ReadAll(r.Body)
				seenBody <- gotBody
				writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
			})
			proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key"}, nil, tr)

			resp, body := postJSON(t, proxy, endpoint, defaultBody, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.StatusCode, body)
			}
			if resp.Header.Get("X-Bursty-Route") != "trustedrouter" || resp.Header.Get("X-Bursty-Reason") != "policy" {
				t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
			}
			assertBurstyBlock(t, body, "trustedrouter", "policy")
			if calls.Load() != 1 {
				t.Fatalf("tr calls = %d, want 1", calls.Load())
			}
			gotBody := <-seenBody
			if !bytes.Equal(gotBody, []byte(defaultBody)) {
				t.Fatalf("tr body = %s, want verbatim %s", gotBody, defaultBody)
			}
		})

		t.Run(endpoint+" provider external forced", func(t *testing.T) {
			t.Parallel()
			tr, trCalls := fakeTR(t)
			proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key"}, nil, tr)

			resp, body := postJSON(t, proxy, endpoint, `{"model":"x","provider":{"order":["anthropic"]},"messages":[]}`, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.StatusCode, body)
			}
			if resp.Header.Get("X-Bursty-Route") != "trustedrouter" || resp.Header.Get("X-Bursty-Reason") != "forced" {
				t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
			}
			if trCalls.Load() != 1 {
				t.Fatalf("tr calls = %d, want 1", trCalls.Load())
			}
		})

		for _, body := range []string{
			`{"model":"x","provider":{"only":["local"]},"messages":[]}`,
			`{"model":"local/x","messages":[]}`,
		} {
			t.Run(endpoint+" local forced rejects "+body, func(t *testing.T) {
				t.Parallel()
				tr, trCalls := fakeTR(t)
				proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key"}, nil, tr)

				resp, got := postJSON(t, proxy, endpoint, body, "")
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d body=%s", resp.StatusCode, got)
				}
				if resp.Header.Get("X-Bursty-Route") != "local" || resp.Header.Get("X-Bursty-Reason") != "forced" {
					t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
				}
				if !bytes.Contains(got, []byte("endpoint_not_supported")) {
					t.Fatalf("body = %s", got)
				}
				if trCalls.Load() != 0 {
					t.Fatalf("tr calls = %d, want 0", trCalls.Load())
				}
			})
		}
	}
}

func TestFailClosedMissingPinnedUpstreams(t *testing.T) {
	t.Run("local pin without local never reaches trustedrouter", func(t *testing.T) {
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key"}, nil, tr)

		resp, body := postChat(t, proxy, `{"model":"local/llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		if resp.Header.Get("X-Bursty-Route") != "local" || resp.Header.Get("X-Bursty-Reason") != "policy" {
			t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
		}
		if !bytes.Contains(body, []byte("local upstream is not configured; request is pinned to local")) {
			t.Fatalf("body = %s", body)
		}
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
	})

	for _, directive := range []string{
		`{"only":["anthropic"]}`,
		`{"order":["openai"]}`,
	} {
		t.Run("non-local provider without trustedrouter "+directive, func(t *testing.T) {
			local, localCalls := fakeLocal(t)
			proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)

			resp, body := postChat(t, proxy, `{"model":"llama3","provider":`+directive+`,"messages":[]}`, "")
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d body=%s", resp.StatusCode, body)
			}
			if resp.Header.Get("X-Bursty-Route") != "trustedrouter" || resp.Header.Get("X-Bursty-Reason") != "policy" {
				t.Fatalf("route headers = %s/%s", resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
			}
			if !bytes.Contains(body, []byte("TrustedRouter is not configured; request requires providers")) {
				t.Fatalf("body = %s", body)
			}
			if localCalls.Load() != 0 {
				t.Fatalf("local calls = %d, want 0", localCalls.Load())
			}
		})
	}
}

func TestForwardedHeadersSanitizedEndToEnd(t *testing.T) {
	headers := http.Header{
		"Authorization":             {"Bearer inbound"},
		"Cookie":                    {"session=secret"},
		"X-Api-Key":                 {"secret"},
		"X-TrustedRouter-Workspace": {"smuggled"},
	}

	t.Run("local", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, key := range []string{"Authorization", "Cookie", "X-Api-Key", "X-TrustedRouter-Workspace"} {
				if got := r.Header.Get(key); got != "" {
					t.Fatalf("%s reached local: %q", key, got)
				}
			}
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		})
		proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)
		resp, body := postChatWithHeaders(t, proxy, `{"model":"llama3","messages":[]}`, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
	})

	t.Run("trustedrouter", func(t *testing.T) {
		tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer tr-key" {
				t.Fatalf("Authorization = %q, want SDK bearer", got)
			}
			for _, key := range []string{"Cookie", "X-Api-Key", "X-TrustedRouter-Workspace"} {
				if got := r.Header.Get(key); got != "" {
					t.Fatalf("%s reached trustedrouter: %q", key, got)
				}
			}
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		})
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, nil, tr)
		resp, body := postChatWithHeaders(t, proxy, `{"model":"trustedrouter/auto","provider":{"order":["anthropic"]},"messages":[]}`, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
	})
}

func TestDynamicHopByHopResponseHeadersDropped(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "X-Secret")
		w.Header().Set("X-Secret", "do-not-forward")
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	})
	proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)

	resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Connection"); got != "" {
		t.Fatalf("Connection = %q, want dropped", got)
	}
	if got := resp.Header.Get("X-Secret"); got != "" {
		t.Fatalf("X-Secret = %q, want dropped", got)
	}
}

func TestDefaultLocalStripsProviderAndBurstsUseOriginalBody(t *testing.T) {
	raw := `{"model":"llama3","provider":{"sort":"price"},"messages":[]}`
	localWant := []byte(`{"model":"llama3","messages":[]}`)

	t.Run("default local strips provider", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			assertJSONEqual(t, body, localWant)
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		})
		tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("trustedrouter should not be called")
		})
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, raw, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "local", "policy")
	})

	t.Run("burst full sends original body to trustedrouter", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { close(releaseLocal) })
		}
		defer release()

		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			assertJSONEqual(t, body, localWant)
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		})
		trBody := make(chan []byte, 1)
		tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			trBody <- body
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		})
		proxy := newProxyWithHandlers(t, config.Config{
			TRAPIKey:            "tr-key",
			LocalMaxConcurrency: 1,
			BurstOnError:        true,
		}, local, tr)

		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			resp, body := postChat(t, proxy, raw, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("first status = %d body=%s", resp.StatusCode, body)
			}
		}()
		<-enteredLocal

		resp, body := postChat(t, proxy, raw, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("burst status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "trustedrouter", "burst-full")
		if got := <-trBody; !bytes.Equal(got, []byte(raw)) {
			t.Fatalf("trustedrouter body = %s, want verbatim %s", got, raw)
		}
		release()
		<-firstDone
	})

	t.Run("burst error sends original body to trustedrouter", func(t *testing.T) {
		var localBody, trBody []byte
		var bodyMu sync.Mutex
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			bodyMu.Lock()
			localBody = body
			bodyMu.Unlock()
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
		})
		tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			bodyMu.Lock()
			trBody = body
			bodyMu.Unlock()
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		})
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, raw, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "trustedrouter", "burst-error")

		bodyMu.Lock()
		gotLocal := append([]byte(nil), localBody...)
		gotTR := append([]byte(nil), trBody...)
		bodyMu.Unlock()
		assertJSONEqual(t, gotLocal, localWant)
		if !bytes.Equal(gotTR, []byte(raw)) {
			t.Fatalf("trustedrouter body = %s, want verbatim %s", gotTR, raw)
		}
	})
}

func TestBurstOnFullReleasesSemaphore(t *testing.T) {
	var trCalls atomic.Int64
	enteredLocal := make(chan struct{})
	releaseLocal := make(chan struct{})
	var localCalls atomic.Int64

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := localCalls.Add(1)
		if call == 1 {
			close(enteredLocal)
			<-releaseLocal
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	})
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trCalls.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	})
	proxy := newProxyWithHandlers(t, config.Config{
		TRAPIKey:            "tr-key",
		LocalMaxConcurrency: 1,
		BurstOnError:        true,
	}, local, tr)

	firstDone := make(chan struct{})
	go func() {
		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("first status = %d", resp.StatusCode)
		}
		assertBurstyBlock(t, body, "local", "policy")
		close(firstDone)
	}()
	<-enteredLocal

	secondResp, secondBody := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if secondResp.Header.Get("X-Bursty-Reason") != "burst-full" {
		t.Fatalf("second reason = %s", secondResp.Header.Get("X-Bursty-Reason"))
	}
	assertBurstyBlock(t, secondBody, "trustedrouter", "burst-full")

	close(releaseLocal)
	<-firstDone

	thirdResp, thirdBody := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if thirdResp.Header.Get("X-Bursty-Route") != "local" {
		t.Fatalf("third route = %s", thirdResp.Header.Get("X-Bursty-Route"))
	}
	assertBurstyBlock(t, thirdBody, "local", "policy")
	if trCalls.Load() != 1 {
		t.Fatalf("tr calls = %d, want 1", trCalls.Load())
	}
	if localCalls.Load() != 2 {
		t.Fatalf("local calls = %d, want 2", localCalls.Load())
	}
}

func TestAllLocalOnlyDoesNotBurstButOrderLocalCanBurstWhenFull(t *testing.T) {
	enteredLocal := make(chan struct{})
	releaseLocal := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseLocal) })
	}
	defer release()

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(enteredLocal) })
		<-releaseLocal
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	})
	var trCalls atomic.Int64
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trCalls.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	})
	proxy := newProxyWithHandlers(t, config.Config{
		TRAPIKey:            "tr-key",
		LocalMaxConcurrency: 1,
		BurstOnError:        true,
	}, local, tr)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("first status = %d body=%s", resp.StatusCode, body)
		}
	}()
	<-enteredLocal

	onlyResp, onlyBody := postChat(t, proxy, `{"model":"llama3","provider":{"only":["local","local"]},"messages":[]}`, "")
	if onlyResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("only-local status = %d body=%s", onlyResp.StatusCode, onlyBody)
	}
	if onlyResp.Header.Get("X-Bursty-Route") != "local" {
		t.Fatalf("only-local route = %s", onlyResp.Header.Get("X-Bursty-Route"))
	}
	if trCalls.Load() != 0 {
		t.Fatalf("tr calls after only-local = %d, want 0", trCalls.Load())
	}

	orderResp, orderBody := postChat(t, proxy, `{"model":"llama3","provider":{"order":["local"]},"messages":[]}`, "")
	if orderResp.StatusCode != http.StatusOK {
		t.Fatalf("order-local status = %d body=%s", orderResp.StatusCode, orderBody)
	}
	assertBurstyBlock(t, orderBody, "trustedrouter", "burst-full")
	if trCalls.Load() != 1 {
		t.Fatalf("tr calls after order-local = %d, want 1", trCalls.Load())
	}

	release()
	<-firstDone
}

func TestBurstOnError(t *testing.T) {
	t.Run("connect refused", func(t *testing.T) {
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithTransports(t, config.Config{
			LocalURL:            "http://local.test",
			TRAPIKey:            "tr-key",
			TRBaseURL:           "http://tr.test/v1",
			LocalMaxConcurrency: 4,
			BurstOnError:        true,
		}, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connect: refused")
		}), handlerTransport{handler: tr})

		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.Header.Get("X-Bursty-Reason") != "burst-error" {
			t.Fatalf("reason = %s", resp.Header.Get("X-Bursty-Reason"))
		}
		assertBurstyBlock(t, body, "trustedrouter", "burst-error")
		if trCalls.Load() != 1 {
			t.Fatalf("tr calls = %d, want 1", trCalls.Load())
		}
	})

	t.Run("status 500", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "trustedrouter", "burst-error")
		if trCalls.Load() != 1 {
			t.Fatalf("tr calls = %d, want 1", trCalls.Load())
		}
	})

	t.Run("forced local status 500 does not burst", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, `{"model":"llama3","provider":{"only":["local"]},"messages":[]}`, "")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "local", "forced")
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
	})

	t.Run("404 model not found", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`model llama3 not found`))
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "trustedrouter", "burst-error")
		if trCalls.Load() != 1 {
			t.Fatalf("tr calls = %d, want 1", trCalls.Load())
		}
	})

	t.Run("404 json error message is the only model sniff source", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"message": "model mistral not found"},
				"debug": "llama3 appears outside the error message",
			})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "local", "policy")
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
	})

	t.Run("404 fallback ignores short model substrings", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`v1 is unavailable`))
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, body := postChat(t, proxy, `{"model":"v1","messages":[]}`, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
	})

	t.Run("disabled surfaces local response", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: false}, local, tr)

		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		assertBurstyBlock(t, body, "local", "policy")
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
	})
}

func TestBurstOnErrorReleasesLocalSlotBeforeTrustedRouterCompletes(t *testing.T) {
	enteredTR := make(chan struct{})
	releaseTR := make(chan struct{})
	secondLocal := make(chan struct{})
	var releaseTROnce sync.Once
	releaseTrustedRouter := func() {
		releaseTROnce.Do(func() { close(releaseTR) })
	}
	defer releaseTrustedRouter()

	var localCalls atomic.Int64
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch localCalls.Add(1) {
		case 1:
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
		case 2:
			close(secondLocal)
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		default:
			t.Errorf("unexpected local call")
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		}
	})

	var enterTROnce sync.Once
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enterTROnce.Do(func() { close(enteredTR) })
		<-releaseTR
		_, _ = w.Write([]byte(`{"id":"tr"}`))
	})
	proxy := newProxyWithHandlers(t, config.Config{
		TRAPIKey:            "tr-key",
		LocalMaxConcurrency: 1,
		BurstOnError:        true,
	}, local, tr)

	firstErr := make(chan error, 1)
	go func() {
		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			firstErr <- fmt.Errorf("first status = %d body=%s", resp.StatusCode, body)
			return
		}
		if resp.Header.Get("X-Bursty-Reason") != "burst-error" {
			firstErr <- fmt.Errorf("first reason = %s", resp.Header.Get("X-Bursty-Reason"))
			return
		}
		firstErr <- nil
	}()

	select {
	case <-enteredTR:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not enter TrustedRouter fallback")
	}

	resp, body := postChat(t, proxy, `{"model":"llama3","provider":{"only":["local"]},"messages":[]}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d body=%s", resp.StatusCode, body)
	}
	assertBurstyBlock(t, body, "local", "forced")
	select {
	case <-secondLocal:
	default:
		t.Fatal("second request was not served by local")
	}

	releaseTrustedRouter()
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish after releasing TrustedRouter")
	}
}

func TestBurstOnErrorDrainsLocalBodyBeforeTrustedRouter(t *testing.T) {
	closed := make(chan struct{})
	var closeOnce sync.Once
	localBody := closeNotifyReadCloser{
		Reader: strings.NewReader(`local failed`),
		close: func() {
			closeOnce.Do(func() { close(closed) })
		},
	}
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-closed:
		default:
			t.Fatal("local response body was not closed before TrustedRouter fallback")
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	})
	proxy := newProxyWithTransports(t, config.Config{
		LocalURL:     "http://local.test",
		TRAPIKey:     "tr-key",
		TRBaseURL:    "http://tr.test/v1",
		BurstOnError: true,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       localBody,
		}, nil
	}), handlerTransport{handler: tr})

	resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	assertBurstyBlock(t, body, "trustedrouter", "burst-error")
}

func TestUpstreamReadErrorClearsStaleHeaders(t *testing.T) {
	proxy := newProxyWithTransports(t, config.Config{
		LocalURL:     "http://local.test",
		BurstOnError: false,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type":     {"application/json"},
				"Content-Length":   {"5000"},
				"Content-Encoding": {"gzip"},
			},
			Body: &errAfterReader{data: []byte(`{"partial":`)},
		}, nil
	}), nil)

	resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want cleared", got)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want cleared", got)
	}
	assertErrorEnvelope(t, body)
}

func TestInjectedJSONResponseRecomputesContentLengthAndClearsEncoding(t *testing.T) {
	proxy := newProxyWithTransports(t, config.Config{
		LocalURL:     "http://local.test",
		BurstOnError: false,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header: http.Header{
				"Content-Type":     {"application/json"},
				"Content-Length":   {"5000"},
				"Content-Encoding": {"gzip"},
			},
			Body: io.NopCloser(strings.NewReader(`{"id":"local"}`)),
		}, nil
	}), nil)

	resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("Content-Length"), fmt.Sprint(len(body)); got != want {
		t.Fatalf("Content-Length = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want cleared", got)
	}
	assertBurstyBlock(t, body, "local", "policy")
}

func TestZeroBytesSentGuardDoesNotRetryAfterStreamingStarts(t *testing.T) {
	var trCalls atomic.Int64
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: half\n\n"))
		w.(http.Flusher).Flush()
	})
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: tr\n\n"))
	})
	proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

	resp, body := postChat(t, proxy, `{"model":"llama3","stream":true,"messages":[]}`, "")
	if resp.Header.Get("X-Bursty-Route") != "local" {
		t.Fatalf("route = %s", resp.Header.Get("X-Bursty-Route"))
	}
	if string(body) != "data: half\n\n" {
		t.Fatalf("stream body = %q", body)
	}
	if trCalls.Load() != 0 {
		t.Fatalf("tr calls = %d, want 0", trCalls.Load())
	}
}

func TestStreamingHeadersFlushBeforeFirstBodyRead(t *testing.T) {
	releaseBody := make(chan struct{})
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-releaseBody
		_, _ = w.Write([]byte("data: ready\n\n"))
		w.(http.Flusher).Flush()
	})
	proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)

	type opened struct {
		resp *http.Response
		done func()
	}
	openedCh := make(chan opened, 1)
	go func() {
		resp, done := openChat(t, proxy, `{"model":"llama3","stream":true,"messages":[]}`, "")
		openedCh <- opened{resp: resp, done: done}
	}()

	var got opened
	select {
	case got = <-openedCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stream headers were not forwarded before the first body chunk")
	}
	defer got.done()
	close(releaseBody)
	reader := bufio.NewReader(got.resp.Body)
	if event := readSSEEvent(t, reader); event != "data: ready\n\n" {
		t.Fatalf("event = %q", event)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		firstFlushed := make(chan struct{})
		allowSecond := make(chan struct{})
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			close(firstFlushed)
			<-allowSecond
			_, _ = w.Write([]byte("data: two\n\n"))
			w.(http.Flusher).Flush()
		})
		tr, _ := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, done := openChat(t, proxy, `{"model":"llama3","stream":true,"messages":[]}`, "")
		defer done()
		reader := bufio.NewReader(resp.Body)
		gotFirst := readSSEEvent(t, reader)
		if gotFirst != "data: one\n\n" {
			t.Fatalf("first event = %q", gotFirst)
		}
		<-firstFlushed
		close(allowSecond)
		gotSecond := readSSEEvent(t, reader)
		if gotSecond != "data: two\n\n" {
			t.Fatalf("second event = %q", gotSecond)
		}
	})

	t.Run("trustedrouter", func(t *testing.T) {
		firstFlushed := make(chan struct{})
		allowSecond := make(chan struct{})
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("local should not be called")
		})
		tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			close(firstFlushed)
			<-allowSecond
			_, _ = w.Write([]byte("data: two\n\n"))
			w.(http.Flusher).Flush()
		})
		proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

		resp, done := openChat(t, proxy, `{"model":"trustedrouter/auto","stream":true,"provider":{"order":["anthropic"]},"messages":[]}`, "")
		defer done()
		reader := bufio.NewReader(resp.Body)
		gotFirst := readSSEEvent(t, reader)
		if gotFirst != "data: one\n\n" {
			t.Fatalf("first event = %q", gotFirst)
		}
		<-firstFlushed
		close(allowSecond)
		gotSecond := readSSEEvent(t, reader)
		if gotSecond != "data: two\n\n" {
			t.Fatalf("second event = %q", gotSecond)
		}
	})
}

func TestTrustedRouterOnlyStreamingPassthrough(t *testing.T) {
	for _, endpoint := range []string{messagesPath, responsesPath} {
		t.Run(endpoint, func(t *testing.T) {
			firstFlushed := make(chan struct{})
			allowSecond := make(chan struct{})
			tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != endpoint {
					t.Fatalf("tr path = %s, want %s", r.URL.Path, endpoint)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: one\n\n"))
				w.(http.Flusher).Flush()
				close(firstFlushed)
				<-allowSecond
				_, _ = w.Write([]byte("data: two\n\n"))
				w.(http.Flusher).Flush()
			})
			proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key"}, nil, tr)

			resp, done := openJSON(t, proxy, endpoint, `{"model":"x","stream":true,"messages":[]}`, "")
			defer done()
			if resp.Header.Get("X-Bursty-Route") != "trustedrouter" {
				t.Fatalf("route = %s", resp.Header.Get("X-Bursty-Route"))
			}
			reader := bufio.NewReader(resp.Body)
			gotFirst := readSSEEvent(t, reader)
			if gotFirst != "data: one\n\n" {
				t.Fatalf("first event = %q", gotFirst)
			}
			<-firstFlushed
			close(allowSecond)
			gotSecond := readSSEEvent(t, reader)
			if gotSecond != "data: two\n\n" {
				t.Fatalf("second event = %q", gotSecond)
			}
		})
	}
}

func TestTrustedRouterOnlyFailClosed(t *testing.T) {
	local, localCalls := fakeLocal(t)
	proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)

	for _, endpoint := range []string{messagesPath, responsesPath} {
		resp, body := postJSON(t, proxy, endpoint, `{"model":"x","messages":[]}`, "")
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s status = %d body=%s", endpoint, resp.StatusCode, body)
		}
		if resp.Header.Get("X-Bursty-Route") != "trustedrouter" || resp.Header.Get("X-Bursty-Reason") != "policy" {
			t.Fatalf("%s route headers = %s/%s", endpoint, resp.Header.Get("X-Bursty-Route"), resp.Header.Get("X-Bursty-Reason"))
		}
		if !bytes.Contains(body, []byte("endpoint_not_supported")) || !bytes.Contains(body, []byte("local-only mode")) {
			t.Fatalf("%s body = %s", endpoint, body)
		}
	}
	if localCalls.Load() != 0 {
		t.Fatalf("local calls = %d, want 0", localCalls.Load())
	}
}

func TestEmbeddingsBurstOnFull(t *testing.T) {
	enteredLocal := make(chan struct{})
	releaseLocal := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseLocal) })
	}
	defer release()

	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("local path = %s", r.URL.Path)
		}
		enterOnce.Do(func() { close(enteredLocal) })
		<-releaseLocal
		writeTestJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
	})
	var trCalls atomic.Int64
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("tr path = %s", r.URL.Path)
		}
		trCalls.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
	})
	proxy := newProxyWithHandlers(t, config.Config{
		TRAPIKey:            "tr-key",
		LocalMaxConcurrency: 1,
		BurstOnError:        true,
	}, local, tr)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		resp, body := postJSON(t, proxy, embeddingsPath, `{"model":"nomic-embed","input":"hello"}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("first status = %d body=%s", resp.StatusCode, body)
		}
	}()
	<-enteredLocal

	resp, body := postJSON(t, proxy, embeddingsPath, `{"model":"nomic-embed","input":"hello"}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("burst status = %d body=%s", resp.StatusCode, body)
	}
	assertBurstyBlock(t, body, "trustedrouter", "burst-full")
	if trCalls.Load() != 1 {
		t.Fatalf("tr calls = %d, want 1", trCalls.Load())
	}

	release()
	<-firstDone
}

func TestEmbeddingsBurstOn404ModelNotFound(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "model nomic-embed not found"},
		})
	})
	tr, trCalls := fakeTR(t)
	proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

	resp, body := postJSON(t, proxy, embeddingsPath, `{"model":"nomic-embed","input":"hello"}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	assertBurstyBlock(t, body, "trustedrouter", "burst-error")
	if trCalls.Load() != 1 {
		t.Fatalf("tr calls = %d, want 1", trCalls.Load())
	}
}

func TestLocalOnlyModeFullReturns429(t *testing.T) {
	enteredLocal := make(chan struct{})
	releaseLocal := make(chan struct{})
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(enteredLocal)
		<-releaseLocal
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	})
	proxy := newProxyWithHandlers(t, config.Config{LocalMaxConcurrency: 1, BurstOnError: true}, local, nil)

	firstDone := make(chan struct{})
	go func() {
		resp, _ := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("first status = %d", resp.StatusCode)
		}
		close(firstDone)
	}()
	<-enteredLocal

	resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	if resp.Header.Get("X-Bursty-Reason") != "burst-full" {
		t.Fatalf("reason = %s", resp.Header.Get("X-Bursty-Reason"))
	}
	assertErrorEnvelope(t, body)
	close(releaseLocal)
	<-firstDone
}

func TestLocalQueueWait(t *testing.T) {
	t.Run("wait then acquire succeeds", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{
			TRAPIKey:            "tr-key",
			LocalMaxConcurrency: 1,
			LocalQueueWait:      500 * time.Millisecond,
			BurstOnError:        true,
		}, local, tr)

		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("first status = %d body=%s", resp.StatusCode, body)
			}
		}()
		<-enteredLocal

		secondDone := make(chan struct{})
		var secondResp *http.Response
		var secondBody []byte
		go func() {
			defer close(secondDone)
			secondResp, secondBody = postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		}()
		time.Sleep(50 * time.Millisecond)
		close(releaseLocal)
		<-firstDone
		<-secondDone

		if secondResp.StatusCode != http.StatusOK {
			t.Fatalf("second status = %d body=%s", secondResp.StatusCode, secondBody)
		}
		assertBurstyBlock(t, secondBody, "local", "policy")
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
	})

	t.Run("wait expires bursts full", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{
			TRAPIKey:            "tr-key",
			LocalMaxConcurrency: 1,
			LocalQueueWait:      20 * time.Millisecond,
			BurstOnError:        true,
		}, local, tr)

		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			_, _ = postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		}()
		<-enteredLocal

		resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertBurstyBlock(t, body, "trustedrouter", "burst-full")
		if trCalls.Load() != 1 {
			t.Fatalf("tr calls = %d, want 1", trCalls.Load())
		}
		close(releaseLocal)
		<-firstDone
	})

	t.Run("cancel while queued does not burst", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
		})
		tr, trCalls := fakeTR(t)
		proxy := newProxyWithHandlers(t, config.Config{
			TRAPIKey:            "tr-key",
			LocalMaxConcurrency: 1,
			LocalQueueWait:      500 * time.Millisecond,
			BurstOnError:        true,
		}, local, tr)

		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			_, _ = postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
		}()
		<-enteredLocal

		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodPost, "http://bursty.test/v1/chat/completions", strings.NewReader(`{"model":"llama3","messages":[]}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			proxy.ServeHTTP(rec, req)
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("canceled queued request did not return")
		}
		if trCalls.Load() != 0 {
			t.Fatalf("tr calls = %d, want 0", trCalls.Load())
		}
		if got := proxy.stats.burstsFull.Value(); got != 0 {
			t.Fatalf("bursts_full = %d, want 0", got)
		}
		close(releaseLocal)
		<-firstDone
	})
}

func TestLocalConcurrencyCapParallel(t *testing.T) {
	const requests = 16
	var current, maxSeen, served atomic.Int64
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := current.Add(1)
		updateMax(&maxSeen, now)
		time.Sleep(10 * time.Millisecond)
		current.Add(-1)
		served.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	})
	proxy := newProxyWithHandlers(t, config.Config{
		LocalMaxConcurrency: 2,
		LocalQueueWait:      2 * time.Second,
		BurstOnError:        true,
	}, local, nil)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d body=%s", resp.StatusCode, body)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("max concurrent local = %d, want <= 2", got)
	}
	if got := served.Load(); got != requests {
		t.Fatalf("served = %d, want %d", got, requests)
	}
}

func TestModelsMergeAndTrustedRouterCache(t *testing.T) {
	var trHits, localHits atomic.Int64
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("local path = %s", r.URL.Path)
		}
		localHits.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "llama3", "object": "model"},
				{"id": "mistral"},
			},
		})
	})
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("tr path = %s", r.URL.Path)
		}
		trHits.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "tr/model", "object": "model", "owned_by": "trustedrouter"},
			},
		})
	})
	proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

	for i := 0; i < 2; i++ {
		resp, body := get(t, proxy, "/v1/models", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		ids := modelIDs(t, body)
		for _, id := range []string{"tr/model", "llama3", "local/llama3", "mistral", "local/mistral"} {
			if !ids[id] {
				t.Fatalf("missing model id %q in %#v", id, ids)
			}
		}
	}
	if trHits.Load() != 1 {
		t.Fatalf("tr hits = %d, want 1", trHits.Load())
	}
	if localHits.Load() != 2 {
		t.Fatalf("local hits = %d, want 2", localHits.Load())
	}
}

func TestModelsTrustedRouterCatalogFallbackAndStale(t *testing.T) {
	var trHits, catalogHits, catalogFail atomic.Int64
	catalog := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("catalog path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("catalog Authorization = %q, want empty", got)
		}
		catalogHits.Add(1)
		if catalogFail.Load() != 0 {
			writeTestJSON(w, http.StatusInternalServerError, map[string]any{"error": "catalog down"})
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "tr/catalog-model", "object": "model", "owned_by": "trustedrouter"},
			},
		})
	})

	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("tr path = %s", r.URL.Path)
		}
		trHits.Add(1)
		writeTestJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "not found"},
		})
	})
	proxy := newProxyWithHandlers(t, config.Config{
		TRAPIKey:     "tr-key",
		TRCatalogURL: "http://catalog.test/v1",
		BurstOnError: true,
	}, nil, tr)
	proxy.catalog = &http.Client{Transport: handlerTransport{handler: catalog}}

	for i := 0; i < 2; i++ {
		resp, body := get(t, proxy, "/v1/models", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		if ids := modelIDs(t, body); !ids["tr/catalog-model"] {
			t.Fatalf("missing catalog model in %#v", ids)
		}
	}
	if trHits.Load() != 1 || catalogHits.Load() != 1 {
		t.Fatalf("cached hits tr=%d catalog=%d, want 1/1", trHits.Load(), catalogHits.Load())
	}

	catalogFail.Store(1)
	proxy.models.mu.Lock()
	proxy.models.expires = time.Now().Add(-time.Second)
	proxy.models.mu.Unlock()

	resp, body := get(t, proxy, "/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale status = %d body=%s", resp.StatusCode, body)
	}
	if ids := modelIDs(t, body); !ids["tr/catalog-model"] {
		t.Fatalf("missing stale catalog model in %#v", ids)
	}
	if trHits.Load() != 2 || catalogHits.Load() != 2 {
		t.Fatalf("stale hits tr=%d catalog=%d, want 2/2", trHits.Load(), catalogHits.Load())
	}

	resp, body = get(t, proxy, "/stats", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d body=%s", resp.StatusCode, body)
	}
	var statsPayload struct {
		CatalogErrors int64 `json:"catalog_errors"`
	}
	if err := json.Unmarshal(body, &statsPayload); err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, body)
	}
	if statsPayload.CatalogErrors != 1 {
		t.Fatalf("catalog_errors = %d, want 1; stats=%s", statsPayload.CatalogErrors, body)
	}
}

func TestModelsEmptyMergeMarshalsEmptyDataArray(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("local path = %s", r.URL.Path)
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{}})
	})
	proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)

	resp, body := get(t, proxy, "/v1/models", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(`"data":null`)) || !bytes.Contains(body, []byte(`"data":[]`)) {
		t.Fatalf("models data should marshal as [] not null: %s", body)
	}
}

func TestStatsEndpointRouteCounters(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "chat-local"})
		case "/v1/embeddings":
			writeTestJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		default:
			t.Fatalf("local path = %s", r.URL.Path)
		}
	})
	tr := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages", "/v1/responses":
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		default:
			t.Fatalf("tr path = %s", r.URL.Path)
		}
	})
	proxy := newProxyWithHandlers(t, config.Config{TRAPIKey: "tr-key", BurstOnError: true}, local, tr)

	if resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d body=%s", resp.StatusCode, body)
	}
	if resp, body := postJSON(t, proxy, embeddingsPath, `{"model":"nomic-embed","input":"hello"}`, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("embeddings status = %d body=%s", resp.StatusCode, body)
	}
	if resp, body := postJSON(t, proxy, messagesPath, `{"model":"anthropic/claude-haiku-4.5","messages":[]}`, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("messages status = %d body=%s", resp.StatusCode, body)
	}
	if resp, body := postJSON(t, proxy, responsesPath, `{"model":"openai/gpt-4.1-mini","input":"hello"}`, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d body=%s", resp.StatusCode, body)
	}

	resp, body := get(t, proxy, "/stats", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d body=%s", resp.StatusCode, body)
	}
	var payload struct {
		Routes         map[string]int64            `json:"routes"`
		EndpointRoutes map[string]map[string]int64 `json:"endpoint_routes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, body)
	}
	if payload.Routes["local"] != 2 || payload.Routes["trustedrouter"] != 2 {
		t.Fatalf("routes = %#v, want local=2 trustedrouter=2", payload.Routes)
	}
	want := map[string]map[string]int64{
		"chat_completions": {"local": 1, "trustedrouter": 0},
		"embeddings":       {"local": 1, "trustedrouter": 0},
		"messages":         {"local": 0, "trustedrouter": 1},
		"responses":        {"local": 0, "trustedrouter": 1},
	}
	for endpoint, routes := range want {
		for route, count := range routes {
			if got := payload.EndpointRoutes[endpoint][route]; got != count {
				t.Fatalf("endpoint_routes[%s][%s] = %d, want %d; payload=%#v", endpoint, route, got, count, payload.EndpointRoutes)
			}
		}
	}
}

func TestBurstyJSONInjectionAbsentForStreaming(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var view struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&view)
		if view.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"local\"}\n\n"))
			return
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	})
	proxy := newProxyWithHandlers(t, config.Config{BurstOnError: true}, local, nil)

	_, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	assertBurstyBlock(t, body, "local", "policy")

	_, streamBody := postChat(t, proxy, `{"model":"llama3","stream":true,"messages":[]}`, "")
	if bytes.Contains(streamBody, []byte("bursty")) {
		t.Fatalf("stream response contains bursty block: %s", streamBody)
	}
}

func TestAcceptEncodingDroppedAndInjectedJSONIsPlaintext(t *testing.T) {
	proxy := newProxyWithTransports(t, config.Config{
		LocalURL:     "http://local.test",
		BurstOnError: true,
	}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Accept-Encoding"); got != "" {
			t.Fatalf("Accept-Encoding reached local transport: %q", got)
		}
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		_, _ = gz.Write([]byte(`{"id":"local"}`))
		_ = gz.Close()
		decoded, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
		if err != nil {
			t.Fatalf("gzip.NewReader() error = %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: readCloser{
				Reader: decoded,
				Closer: decoded,
			},
			Request: r,
		}, nil
	}), nil)
	resp, body := postChatWithHeaders(t, proxy, `{"model":"llama3","messages":[]}`, http.Header{
		"Accept-Encoding": {"gzip"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want identity", got)
	}
	assertBurstyBlock(t, body, "local", "policy")
}

func TestTokenAuth(t *testing.T) {
	local, _ := fakeLocal(t)
	proxy := newProxyWithHandlers(t, config.Config{
		BurstOnError: true,
		Token:        "secret",
	}, local, nil)

	resp, body := postChat(t, proxy, `{"model":"llama3","messages":[]}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = postChat(t, proxy, `{"model":"llama3","messages":[]}`, "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong auth status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = get(t, proxy, "/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = get(t, proxy, "/stats", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stats no auth status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = get(t, proxy, "/stats", "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats authorized status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = postChat(t, proxy, `{"model":"llama3","messages":[]}`, "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized status = %d body=%s", resp.StatusCode, body)
	}
}

func fakeLocal(t *testing.T) (http.Handler, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "local"})
	}), &calls
}

func fakeTR(t *testing.T) (http.Handler, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeTestJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	}), &calls
}

func newProxyWithHandlers(t *testing.T, cfg config.Config, localHandler, trHandler http.Handler) *Server {
	t.Helper()
	var localTransport http.RoundTripper
	if localHandler != nil {
		localTransport = handlerTransport{handler: localHandler}
	}
	var trTransport http.RoundTripper
	if trHandler != nil {
		trTransport = handlerTransport{handler: trHandler}
	}
	if localHandler != nil && cfg.LocalURL == "" {
		cfg.LocalURL = "http://local.test"
	}
	if trHandler != nil && cfg.TRAPIKey == "" {
		cfg.TRAPIKey = "tr-key"
	}
	if trHandler != nil && cfg.TRBaseURL == "" {
		cfg.TRBaseURL = "http://tr.test/v1"
	}
	return newProxyWithTransports(t, cfg, localTransport, trTransport)
}

func newProxyWithTransports(t *testing.T, cfg config.Config, localTransport, trTransport http.RoundTripper) *Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = ":0"
	}
	if cfg.LocalMaxConcurrency == 0 {
		cfg.LocalMaxConcurrency = 4
	}
	if cfg.TRBaseURL == "" {
		cfg.TRBaseURL = "http://tr.test/v1"
	}

	server := &Server{cfg: cfg, stats: newStats()}
	if cfg.LocalURL != "" {
		local, err := upstream.NewLocalWithHTTPClient(cfg.LocalURL, &http.Client{Transport: localTransport})
		if err != nil {
			t.Fatalf("NewLocalWithHTTPClient() error = %v", err)
		}
		server.local = local
		server.localSlots = make(chan struct{}, cfg.LocalMaxConcurrency)
	}
	if cfg.TRAPIKey != "" {
		tr, err := upstream.NewTrustedRouterWithHTTPClient(cfg.TRAPIKey, cfg.TRBaseURL, &http.Client{Transport: trTransport})
		if err != nil {
			t.Fatalf("NewTrustedRouterWithHTTPClient() error = %v", err)
		}
		server.tr = tr
	}
	return server
}

func postChat(t *testing.T, proxy *Server, body, token string) (*http.Response, []byte) {
	t.Helper()
	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	return postChatWithHeaders(t, proxy, body, headers)
}

func postChatWithHeaders(t *testing.T, proxy *Server, body string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	return postJSONWithHeaders(t, proxy, chatCompletionsPath, body, headers)
}

func postJSON(t *testing.T, proxy *Server, path, body, token string) (*http.Response, []byte) {
	t.Helper()
	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	return postJSONWithHeaders(t, proxy, path, body, headers)
}

func postJSONWithHeaders(t *testing.T, proxy *Server, path, body string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://bursty.test"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for key, values := range headers {
		req.Header.Del(key)
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	resp := rec.Result()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, data
}

func openChat(t *testing.T, proxy *Server, body, token string) (*http.Response, func()) {
	t.Helper()
	return openJSON(t, proxy, chatCompletionsPath, body, token)
}

func openJSON(t *testing.T, proxy *Server, path, body, token string) (*http.Response, func()) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://bursty.test"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return openHandlerResponse(t, proxy, req)
}

func get(t *testing.T, proxy *Server, path, token string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://bursty.test"+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	resp := rec.Result()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, data
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
		b.WriteString(line)
		if line == "\n" {
			return b.String()
		}
	}
}

func assertBurstyBlock(t *testing.T, body []byte, route, reason string) {
	t.Helper()
	var payload struct {
		Bursty struct {
			Route  string `json:"route"`
			Reason string `json:"reason"`
		} `json:"bursty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if payload.Bursty.Route != route || payload.Bursty.Reason != reason {
		t.Fatalf("bursty block = %#v, want %s/%s body=%s", payload.Bursty, route, reason, body)
	}
}

func assertErrorEnvelope(t *testing.T, body []byte) {
	t.Helper()
	var payload struct {
		Error struct {
			Code   string `json:"code"`
			Source string `json:"source"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, body)
	}
	if payload.Error.Code == "" || payload.Error.Source != "bursty" {
		t.Fatalf("bad error envelope: %s", body)
	}
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

func modelIDs(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("models response is not JSON: %v\n%s", err, body)
	}
	out := make(map[string]bool, len(payload.Data))
	for _, model := range payload.Data {
		if strings.HasPrefix(model.ID, "local/") && model.OwnedBy != "local" {
			t.Fatalf("local model %s owned_by = %q", model.ID, model.OwnedBy)
		}
		out[model.ID] = true
	}
	return out
}

func updateMax(max *atomic.Int64, value int64) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
}

type closeNotifyReadCloser struct {
	io.Reader
	close func()
}

func (r closeNotifyReadCloser) Close() error {
	if r.close != nil {
		r.close()
	}
	return nil
}

type errAfterReader struct {
	data []byte
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, errors.New("upstream read failed")
}

func (r *errAfterReader) Close() error {
	return nil
}

type handlerTransport struct {
	handler http.Handler
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.handler == nil {
		return nil, errors.New("no handler")
	}
	return openHandlerResponseForRequest(req, t.handler)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func openHandlerResponse(t *testing.T, handler http.Handler, req *http.Request) (*http.Response, func()) {
	t.Helper()
	resp, done, err := openHandlerResponseWithDone(req, handler)
	if err != nil {
		t.Fatalf("open handler response: %v", err)
	}
	return resp, func() {
		_ = resp.Body.Close()
		done()
	}
}

func openHandlerResponseForRequest(req *http.Request, handler http.Handler) (*http.Response, error) {
	resp, _, err := openHandlerResponseWithDone(req, handler)
	return resp, err
}

func openHandlerResponseWithDone(req *http.Request, handler http.Handler) (*http.Response, func(), error) {
	reader, writer := io.Pipe()
	rw := newPipeResponseWriter(writer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rw, req)
		rw.finish(nil)
	}()

	select {
	case <-rw.headerReady:
		return rw.response(req, reader), func() { <-done }, nil
	case <-req.Context().Done():
		rw.finish(req.Context().Err())
		return nil, func() { <-done }, req.Context().Err()
	}
}

type pipeResponseWriter struct {
	header      http.Header
	pipe        *io.PipeWriter
	headerReady chan struct{}
	readyOnce   sync.Once
	mu          sync.Mutex
	status      int
}

func newPipeResponseWriter(pipe *io.PipeWriter) *pipeResponseWriter {
	return &pipeResponseWriter{
		header:      make(http.Header),
		pipe:        pipe,
		headerReady: make(chan struct{}),
		status:      http.StatusOK,
	}
}

func (w *pipeResponseWriter) Header() http.Header {
	return w.header
}

func (w *pipeResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	} else if !w.headerWritten() {
		w.status = status
	}
	w.mu.Unlock()
	w.readyOnce.Do(func() {
		close(w.headerReady)
	})
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pipe.Write(p)
}

func (w *pipeResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
}

func (w *pipeResponseWriter) finish(err error) {
	w.WriteHeader(http.StatusOK)
	if err != nil {
		_ = w.pipe.CloseWithError(err)
		return
	}
	_ = w.pipe.Close()
}

func (w *pipeResponseWriter) response(req *http.Request, body io.ReadCloser) *http.Response {
	w.mu.Lock()
	status := w.status
	header := w.header.Clone()
	w.mu.Unlock()
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       body,
		Request:    req,
	}
}

func (w *pipeResponseWriter) headerWritten() bool {
	select {
	case <-w.headerReady:
		return true
	default:
		return false
	}
}
