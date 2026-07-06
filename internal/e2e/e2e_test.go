package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	chatPath = "/v1/chat/completions"
)

var (
	burstyBinary string
	repoRoot     string
)

func TestMain(m *testing.M) {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find repo root: %v\n", err)
		os.Exit(2)
	}
	repoRoot = root

	tmp, err := os.MkdirTemp("", "burstyrouter-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp dir: %v\n", err)
		os.Exit(2)
	}
	defer os.RemoveAll(tmp)

	binary := filepath.Join(tmp, "burstyrouter")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/burstyrouter")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GOCACHE="+filepath.Join(tmp, "gocache"),
		"GOTMPDIR="+tmp,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "go build ./cmd/burstyrouter: %v\n%s\n", err, out)
		os.Exit(2)
	}
	burstyBinary = binary

	os.Exit(m.Run())
}

func TestBinaryHealthStatsAndRoutingMatrix(t *testing.T) {
	localLog := &requestLog{}
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localLog.add(r)
		if r.URL.Path != chatPath {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected local path"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
	}))
	defer local.Close()

	trLog := &requestLog{}
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trLog.add(r)
		if r.URL.Path != chatPath {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected trustedrouter path"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: tr.URL + "/v1",
	})

	resp, body := get(t, proc, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", resp.StatusCode, body)
	}
	var health struct {
		OK            bool `json:"ok"`
		Local         bool `json:"local"`
		TrustedRouter bool `json:"trustedrouter"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("healthz JSON: %v\n%s", err, body)
	}
	if !health.OK || !health.Local || !health.TrustedRouter {
		t.Fatalf("healthz = %#v, want ok/local/trustedrouter true", health)
	}

	resp, body = get(t, proc, "/stats", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d body=%s", resp.StatusCode, body)
	}
	assertStatsShape(t, body)

	resp, body = get(t, proc, "/ui", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ui status = %d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`id="savings-odometer"`)) {
		t.Fatalf("ui missing odometer element")
	}
	resp, body = get(t, proc, "/metrics", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("bursty_requests_total")) {
		t.Fatalf("metrics missing requests counter: %s", body)
	}

	defaultBody := `{"model":"llama3","messages":[{"role":"user","content":"hello"}]}`
	resp, body = postChat(t, proc, defaultBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "local", "policy")
	assertBurstyBlock(t, body, "local", "policy")
	if got := localLog.last(t).Body; !bytes.Equal(got, []byte(defaultBody)) {
		t.Fatalf("default local body = %s, want byte-identical %s", got, defaultBody)
	}

	resp, body = postChat(t, proc, `{"model":"local/llama3","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local prefix status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "local", "forced")
	assertBurstyBlock(t, body, "local", "forced")
	assertTopLevelString(t, localLog.last(t).Body, "model", "llama3")

	resp, body = postChat(t, proc, `{"model":"llama3","provider":{"only":["local"]},"messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider.only local status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "local", "forced")
	assertBurstyBlock(t, body, "local", "forced")
	assertNoTopLevelKey(t, localLog.last(t).Body, "provider")

	trBody := `{"model":"trustedrouter/auto","provider":{"order":["anthropic"]},"messages":[{"role":"user","content":"<b>&"}]}`
	resp, body = postChat(t, proc, trBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider.order external status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "trustedrouter", "forced")
	assertBurstyBlock(t, body, "trustedrouter", "forced")
	if got := trLog.last(t).Body; !bytes.Equal(got, []byte(trBody)) {
		t.Fatalf("trustedrouter body = %s, want byte-identical %s", got, trBody)
	}
}

func TestBinaryBurstOnFull(t *testing.T) {
	enteredLocal := make(chan struct{})
	releaseLocal := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseLocal) })
	}
	defer release()

	localLog := &requestLog{}
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localLog.add(r)
		enteredOnce.Do(func() { close(enteredLocal) })
		<-releaseLocal
		writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
	}))
	defer local.Close()

	trLog := &requestLog{}
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trLog.add(r)
		writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		localURL:            local.URL,
		trAPIKey:            "e2e-tr-key",
		trBaseURL:           tr.URL + "/v1",
		trCatalogURL:        tr.URL + "/v1",
		localMaxConcurrency: 1,
	})

	firstDone := make(chan error, 1)
	go func() {
		resp, body, err := postChatResult(proc, `{"model":"openai/gpt-4o","messages":[]}`, nil, 5*time.Second)
		if err != nil {
			firstDone <- err
			return
		}
		if resp.StatusCode != http.StatusOK {
			firstDone <- fmt.Errorf("first status = %d body=%s", resp.StatusCode, body)
			return
		}
		firstDone <- nil
	}()
	select {
	case <-enteredLocal:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach local")
	}

	resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("burst status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "trustedrouter", "burst-full")
	assertBurstyBlock(t, body, "trustedrouter", "burst-full")
	if trLog.count() != 1 {
		t.Fatalf("trustedrouter calls = %d, want 1", trLog.count())
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if localLog.count() != 1 {
		t.Fatalf("local calls = %d, want 1", localLog.count())
	}
}

func TestBinaryAliasRoutingModelsAndBurst(t *testing.T) {
	t.Run("alias rewrites local body and appears in models", func(t *testing.T) {
		localLog := &requestLog{}
		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case chatPath:
				localLog.add(r)
				writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
			case "/v1/models":
				writeJSON(w, http.StatusOK, map[string]any{
					"data": []map[string]any{{"id": "llama3", "object": "model"}},
				})
			default:
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected local path"})
			}
		}))
		defer local.Close()

		proc := startBursty(t, burstyConfig{
			localURL: local.URL,
			aliases:  []string{"gpt-4o=llama3"},
		})

		resp, body := postChat(t, proc, `{"model":"gpt-4o","messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertRoute(t, resp, "local", "policy")
		assertTopLevelString(t, localLog.last(t).Body, "model", "llama3")

		resp, body = get(t, proc, "/v1/models", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("models status = %d body=%s", resp.StatusCode, body)
		}
		ids := modelIDs(t, body)
		if !ids["gpt-4o"] {
			t.Fatalf("missing alias model in %#v; body=%s", ids, body)
		}
	})

	t.Run("alias burst sends original model", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { close(releaseLocal) })
		}
		defer release()

		localLog := &requestLog{}
		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			localLog.add(r)
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
		}))
		defer local.Close()

		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		}))
		defer tr.Close()

		proc := startBursty(t, burstyConfig{
			localURL:            local.URL,
			trAPIKey:            "e2e-tr-key",
			trBaseURL:           tr.URL + "/v1",
			trCatalogURL:        tr.URL + "/v1",
			localMaxConcurrency: 1,
			aliases:             []string{"gpt-4o=llama3"},
		})

		firstDone := make(chan error, 1)
		go func() {
			resp, body, err := postChatResult(proc, `{"model":"gpt-4o","messages":[]}`, nil, 5*time.Second)
			if err != nil {
				firstDone <- err
				return
			}
			if resp.StatusCode != http.StatusOK {
				firstDone <- fmt.Errorf("first status = %d body=%s", resp.StatusCode, body)
				return
			}
			firstDone <- nil
		}()
		select {
		case <-enteredLocal:
		case <-time.After(2 * time.Second):
			t.Fatal("first request did not reach local")
		}
		assertTopLevelString(t, localLog.last(t).Body, "model", "llama3")

		resp, body := postChat(t, proc, `{"model":"gpt-4o","messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("burst status = %d body=%s", resp.StatusCode, body)
		}
		assertRoute(t, resp, "trustedrouter", "burst-full")
		assertTopLevelString(t, trLog.last(t).Body, "model", "gpt-4o")

		release()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
	})
}

func TestBinaryUnmappedLocalModelSuppressionAndFallback(t *testing.T) {
	t.Run("unmapped full returns 429 without trustedrouter call", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { close(releaseLocal) })
		}
		defer release()

		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
		}))
		defer local.Close()

		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		}))
		defer tr.Close()

		proc := startBursty(t, burstyConfig{
			localURL:            local.URL,
			trAPIKey:            "e2e-tr-key",
			trBaseURL:           tr.URL + "/v1",
			trCatalogURL:        tr.URL + "/v1",
			localMaxConcurrency: 1,
		})

		firstDone := make(chan error, 1)
		go func() {
			resp, body, err := postChatResult(proc, `{"model":"llama3.2","messages":[]}`, nil, 5*time.Second)
			if err != nil {
				firstDone <- err
				return
			}
			if resp.StatusCode != http.StatusOK {
				firstDone <- fmt.Errorf("first status = %d body=%s", resp.StatusCode, body)
				return
			}
			firstDone <- nil
		}()
		select {
		case <-enteredLocal:
		case <-time.After(2 * time.Second):
			t.Fatal("first request did not reach local")
		}

		resp, body := postChat(t, proc, `{"model":"llama3.2","messages":[]}`, nil)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertRoute(t, resp, "local", "burst-full")
		if trLog.count() != 0 {
			t.Fatalf("trustedrouter calls = %d, want 0", trLog.count())
		}

		release()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fallback model is sent to trustedrouter", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { close(releaseLocal) })
		}
		defer release()

		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
		}))
		defer local.Close()

		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"model": "openai/gpt-4o-mini"})
		}))
		defer tr.Close()

		proc := startBursty(t, burstyConfig{
			localURL:            local.URL,
			trAPIKey:            "e2e-tr-key",
			trBaseURL:           tr.URL + "/v1",
			trCatalogURL:        tr.URL + "/v1",
			localMaxConcurrency: 1,
			burstFallbackModel:  "openai/gpt-4o-mini",
		})

		firstDone := make(chan error, 1)
		go func() {
			resp, body, err := postChatResult(proc, `{"model":"llama3.2","messages":[]}`, nil, 5*time.Second)
			if err != nil {
				firstDone <- err
				return
			}
			if resp.StatusCode != http.StatusOK {
				firstDone <- fmt.Errorf("first status = %d body=%s", resp.StatusCode, body)
				return
			}
			firstDone <- nil
		}()
		select {
		case <-enteredLocal:
		case <-time.After(2 * time.Second):
			t.Fatal("first request did not reach local")
		}

		resp, body := postChat(t, proc, `{"model":"llama3.2","messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertRoute(t, resp, "trustedrouter", "burst-full")
		assertTopLevelString(t, trLog.last(t).Body, "model", "openai/gpt-4o-mini")

		release()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
	})
}

func TestBinaryBurstOnError(t *testing.T) {
	t.Run("status 500", func(t *testing.T) {
		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
		}))
		defer local.Close()

		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		}))
		defer tr.Close()

		proc := startBursty(t, burstyConfig{
			localURL:     local.URL,
			trAPIKey:     "e2e-tr-key",
			trBaseURL:    tr.URL + "/v1",
			trCatalogURL: tr.URL + "/v1",
		})

		resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertRoute(t, resp, "trustedrouter", "burst-error")
		assertBurstyBlock(t, body, "trustedrouter", "burst-error")
		if trLog.count() != 1 {
			t.Fatalf("trustedrouter calls = %d, want 1", trLog.count())
		}
	})

	t.Run("dead port", func(t *testing.T) {
		deadLocalURL := "http://" + freeAddr(t)
		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		}))
		defer tr.Close()

		proc := startBursty(t, burstyConfig{
			localURL:     deadLocalURL,
			trAPIKey:     "e2e-tr-key",
			trBaseURL:    tr.URL + "/v1",
			trCatalogURL: tr.URL + "/v1",
		})

		resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		assertRoute(t, resp, "trustedrouter", "burst-error")
		assertBurstyBlock(t, body, "trustedrouter", "burst-error")
		if trLog.count() != 1 {
			t.Fatalf("trustedrouter calls = %d, want 1", trLog.count())
		}
	})
}

func TestBinaryForcedLocalFailureDoesNotBurst(t *testing.T) {
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "local failed"})
	}))
	defer local.Close()

	trLog := &requestLog{}
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trLog.add(r)
		writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: tr.URL + "/v1",
	})

	resp, body := postChat(t, proc, `{"model":"local/llama3","messages":[]}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "local", "forced")
	assertErrorEnvelope(t, body)
	if trLog.count() != 0 {
		t.Fatalf("trustedrouter calls = %d, want 0", trLog.count())
	}
}

func TestBinaryStreamingFlushesIncrementally(t *testing.T) {
	allowFirst := make(chan struct{})
	allowSecond := make(chan struct{})
	allowDone := make(chan struct{})
	var firstOnce, secondOnce, doneOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(allowFirst) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(allowSecond) }) }
	releaseDone := func() { doneOnce.Do(func() { close(allowDone) }) }
	defer releaseFirst()
	defer releaseSecond()
	defer releaseDone()

	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		<-allowFirst
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"))
		w.(http.Flusher).Flush()

		<-allowSecond
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"))
		w.(http.Flusher).Flush()

		<-allowDone
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer local.Close()

	proc := startBursty(t, burstyConfig{localURL: local.URL})
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := openChat(t, proc, `{"model":"llama3","stream":true,"messages":[]}`, nil)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	var resp *http.Response
	select {
	case resp = <-respCh:
	case err := <-errCh:
		t.Fatalf("open stream: %v", err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stream headers were not forwarded before the first chunk")
	}
	defer resp.Body.Close()
	assertRoute(t, resp, "local", "policy")
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(resp.Body)
	releaseFirst()
	first := readSSEEventWithin(t, reader, time.Second)
	if !strings.Contains(first, `"content":"one"`) {
		t.Fatalf("first SSE event = %q", first)
	}
	releaseSecond()
	second := readSSEEventWithin(t, reader, time.Second)
	if !strings.Contains(second, `"content":"two"`) {
		t.Fatalf("second SSE event = %q", second)
	}
	releaseDone()
	done := readSSEEventWithin(t, reader, time.Second)
	if done != "data: [DONE]\n\n" {
		t.Fatalf("terminal SSE event = %q", done)
	}
}

func TestBinaryStreamingSavingsAndLocalBodySplice(t *testing.T) {
	localLog := &requestLog{}
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localLog.add(r)
		if r.URL.Path != chatPath {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected local path"})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"llama3\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer local.Close()

	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "not found"}})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "trustedrouter inference should not be called"})
	}))
	defer tr.Close()

	catalog := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected catalog path"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "openai/gpt-4o", "pricing": map[string]any{"prompt": "0.000001", "completion": "0.000002"}},
			},
		})
	}))
	defer catalog.Close()

	proc := startBursty(t, burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: catalog.URL + "/v1",
		aliases:      []string{"openai/gpt-4o=llama3"},
	})

	resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","stream":true,"messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Bursty-Saved-USD"); got == "" {
		t.Fatal("missing X-Bursty-Saved-USD")
	}
	assertTopLevelBool(t, localLog.last(t).Body, "stream_options", "include_usage", true)

	_, statsBody := get(t, proc, "/stats", nil)
	if got := savedUSD(t, statsBody); got != 0.00002 {
		t.Fatalf("saved_usd = %.8f, want 0.00002; stats=%s", got, statsBody)
	}
}

func TestBinaryStreamOptionsNotSplicedIntoBurstBody(t *testing.T) {
	enteredLocal := make(chan struct{})
	releaseLocal := make(chan struct{})
	var enterOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseLocal) })
	}
	defer release()

	localLog := &requestLog{}
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localLog.add(r)
		enterOnce.Do(func() { close(enteredLocal) })
		<-releaseLocal
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer local.Close()

	trLog := &requestLog{}
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trLog.add(r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		localURL:            local.URL,
		trAPIKey:            "e2e-tr-key",
		trBaseURL:           tr.URL + "/v1",
		trCatalogURL:        tr.URL + "/v1",
		localMaxConcurrency: 1,
	})

	firstDone := make(chan error, 1)
	go func() {
		_, _, err := postChatResult(proc, `{"model":"openai/gpt-4o","stream":true,"messages":[]}`, nil, 5*time.Second)
		firstDone <- err
	}()
	select {
	case <-enteredLocal:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach local")
	}
	assertTopLevelBool(t, localLog.last(t).Body, "stream_options", "include_usage", true)

	resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","stream":true,"messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("burst status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "trustedrouter", "burst-full")
	if bytes.Contains(trLog.last(t).Body, []byte("stream_options")) {
		t.Fatalf("trustedrouter body contains local stream_options: %s", trLog.last(t).Body)
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestBinaryCloudControlsAndBudget(t *testing.T) {
	t.Run("explicit blocks automatic burst but allows explicit cloud", func(t *testing.T) {
		enteredLocal := make(chan struct{})
		releaseLocal := make(chan struct{})
		var enterOnce sync.Once
		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enterOnce.Do(func() { close(enteredLocal) })
			<-releaseLocal
			writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
		}))
		defer local.Close()

		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		}))
		defer tr.Close()

		proc := startBursty(t, burstyConfig{
			localURL:            local.URL,
			trAPIKey:            "e2e-tr-key",
			trBaseURL:           tr.URL + "/v1",
			trCatalogURL:        tr.URL + "/v1",
			localMaxConcurrency: 1,
			cloud:               "explicit",
		})

		firstDone := make(chan error, 1)
		go func() {
			_, _, err := postChatResult(proc, `{"model":"openai/gpt-4o","messages":[]}`, nil, 5*time.Second)
			firstDone <- err
		}()
		select {
		case <-enteredLocal:
		case <-time.After(2 * time.Second):
			t.Fatal("first request did not reach local")
		}
		resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","messages":[]}`, nil)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("blocked burst status = %d body=%s", resp.StatusCode, body)
		}
		if trLog.count() != 0 {
			t.Fatalf("trustedrouter calls = %d, want 0", trLog.count())
		}
		close(releaseLocal)
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}

		resp, body = postChat(t, proc, `{"model":"openai/gpt-4o","provider":{"order":["openai"]},"messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("explicit status = %d body=%s", resp.StatusCode, body)
		}
		if trLog.count() != 1 {
			t.Fatalf("trustedrouter calls = %d, want 1", trLog.count())
		}
	})

	t.Run("off blocks explicit cloud", func(t *testing.T) {
		local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
		}))
		defer local.Close()
		trLog := &requestLog{}
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trLog.add(r)
			writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
		}))
		defer tr.Close()
		proc := startBursty(t, burstyConfig{
			localURL:     local.URL,
			trAPIKey:     "e2e-tr-key",
			trBaseURL:    tr.URL + "/v1",
			trCatalogURL: tr.URL + "/v1",
			cloud:        "off",
		})
		resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","provider":{"order":["openai"]},"messages":[]}`, nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d body=%s", resp.StatusCode, body)
		}
		if !bytes.Contains(body, []byte("cloud disabled by -cloud=off")) {
			t.Fatalf("body = %s", body)
		}
		if trLog.count() != 0 {
			t.Fatalf("trustedrouter calls = %d, want 0", trLog.count())
		}
	})

	t.Run("budget blocks second cloud send", func(t *testing.T) {
		tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/models") {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "not found"}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":    "tr",
				"model": "openai/gpt-4o",
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 0},
			})
		}))
		defer tr.Close()
		catalog := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"data": []map[string]any{
					{"id": "openai/gpt-4o", "pricing": map[string]any{"prompt": "0.000001", "completion": "0.000001"}},
				},
			})
		}))
		defer catalog.Close()
		proc := startBursty(t, burstyConfig{
			trAPIKey:      "e2e-tr-key",
			trBaseURL:     tr.URL + "/v1",
			trCatalogURL:  catalog.URL + "/v1",
			maxCloudSpend: "0.000001",
		})
		resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","provider":{"order":["openai"]},"messages":[]}`, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first status = %d body=%s", resp.StatusCode, body)
		}
		resp, body = postChat(t, proc, `{"model":"openai/gpt-4o","provider":{"order":["openai"]},"messages":[]}`, nil)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second status = %d body=%s", resp.StatusCode, body)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
		if !bytes.Contains(body, []byte("cloud_budget_exhausted")) {
			t.Fatalf("body = %s", body)
		}
	})
}

func TestBinaryStatePersistsAcrossRestart(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "bursty", "state.json")
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    "local",
			"model": "llama3",
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer local.Close()
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "not found"}})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "trustedrouter inference should not be called"})
	}))
	defer tr.Close()
	catalog := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "openai/gpt-4o", "pricing": map[string]any{"prompt": "0.000001", "completion": "0.000002"}},
			},
		})
	}))
	defer catalog.Close()
	cfg := burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: catalog.URL + "/v1",
		aliases:      []string{"openai/gpt-4o=llama3"},
		stateFile:    stateFile,
	}

	proc := startBursty(t, cfg)
	resp, body := postChat(t, proc, `{"model":"openai/gpt-4o","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	proc.stop(t)

	proc = startBursty(t, cfg)
	_, statsBody := get(t, proc, "/stats", nil)
	if got := savedUSD(t, statsBody); got != 0.00002 {
		t.Fatalf("saved_usd after restart = %.8f, want 0.00002; stats=%s", got, statsBody)
	}
}

func TestBinarySanitizesInboundHeaders(t *testing.T) {
	localLog := &requestLog{}
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localLog.add(r)
		writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
	}))
	defer local.Close()

	trLog := &requestLog{}
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trLog.add(r)
		writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: tr.URL + "/v1",
	})
	inbound := http.Header{
		"Authorization":             {"Bearer inbound-secret"},
		"Cookie":                    {"session=secret"},
		"X-Api-Key":                 {"inbound-secret"},
		"X-TrustedRouter-Workspace": {"workspace-secret"},
	}

	resp, body := postChat(t, proc, `{"model":"llama3","messages":[]}`, inbound)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local status = %d body=%s", resp.StatusCode, body)
	}
	assertSanitizedLocalHeaders(t, localLog.last(t).Header)

	resp, body = postChat(t, proc, `{"model":"trustedrouter/auto","provider":{"order":["anthropic"]},"messages":[]}`, inbound)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trustedrouter status = %d body=%s", resp.StatusCode, body)
	}
	assertSanitizedTrustedRouterHeaders(t, trLog.last(t).Header, "e2e-tr-key")
}

func TestBinaryTokenAuth(t *testing.T) {
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
	}))
	defer local.Close()

	proc := startBursty(t, burstyConfig{
		localURL: local.URL,
		token:    "secret",
	})

	resp, body := get(t, proc, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = get(t, proc, "/stats", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stats without token status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = postChat(t, proc, `{"model":"llama3","messages":[]}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("chat without token status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = postChat(t, proc, `{"model":"llama3","messages":[]}`, bearer("secret"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat with token status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = get(t, proc, "/stats", bearer("secret"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats with token status = %d body=%s", resp.StatusCode, body)
	}
}

func TestBinaryXAPIKeyAuth(t *testing.T) {
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": "local"})
	}))
	defer local.Close()

	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected trustedrouter path"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "tr"})
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: tr.URL + "/v1",
		token:        "secret",
	})

	resp, body := get(t, proc, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d body=%s", resp.StatusCode, body)
	}
	resp, body = postChat(t, proc, `{"model":"llama3","messages":[]}`, xAPIKey("secret"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat x-api-key status = %d body=%s", resp.StatusCode, body)
	}
	for _, tt := range []struct {
		name   string
		header http.Header
	}{
		{name: "bearer", header: bearer("secret")},
		{name: "x-api-key", header: xAPIKey("secret")},
	} {
		resp, body = do(t, proc, http.MethodPost, "/v1/messages", `{"model":"anthropic/claude-haiku-4.5","messages":[]}`, tt.header)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("messages %s status = %d body=%s", tt.name, resp.StatusCode, body)
		}
	}
	resp, body = do(t, proc, http.MethodPost, "/v1/messages", `{"model":"anthropic/claude-haiku-4.5","messages":[]}`, xAPIKey("wrong"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("messages wrong x-api-key status = %d body=%s", resp.StatusCode, body)
	}
}

func TestBinaryTrustedRouterOnlyUpstream404MapsTo501(t *testing.T) {
	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "missing endpoint"})
	}))
	defer tr.Close()

	proc := startBursty(t, burstyConfig{
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: tr.URL + "/v1",
	})

	for _, tt := range []struct {
		path string
		body string
	}{
		{path: "/v1/messages", body: `{"model":"anthropic/claude-haiku-4.5","messages":[]}`},
		{path: "/v1/responses", body: `{"model":"openai/gpt-4.1-mini","input":"hello"}`},
	} {
		resp, body := do(t, proc, http.MethodPost, tt.path, tt.body, nil)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s status = %d body=%s", tt.path, resp.StatusCode, body)
		}
		assertRoute(t, resp, "trustedrouter", "policy")
		assertErrorEnvelope(t, body)
		if !bytes.Contains(body, []byte("endpoint_not_supported")) {
			t.Fatalf("%s body = %s", tt.path, body)
		}
	}
}

func TestBinaryModelsMergeUsesCatalogAndLocalPrefixes(t *testing.T) {
	local := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected local path"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "llama3", "object": "model"},
				{"id": "mistral"},
			},
		})
	}))
	defer local.Close()

	tr := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected trustedrouter path"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "not found"}})
	}))
	defer tr.Close()

	catalog := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected catalog path"})
			return
		}
		if got := r.Header.Get("Authorization"); got != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "catalog should not receive auth"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"id": "tr/catalog-model", "object": "model", "owned_by": "trustedrouter"},
			},
		})
	}))
	defer catalog.Close()

	proc := startBursty(t, burstyConfig{
		localURL:     local.URL,
		trAPIKey:     "e2e-tr-key",
		trBaseURL:    tr.URL + "/v1",
		trCatalogURL: catalog.URL + "/v1",
	})

	resp, body := get(t, proc, "/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "trustedrouter", "policy")
	ids := modelIDs(t, body)
	for _, id := range []string{"tr/catalog-model", "llama3", "local/llama3", "mistral", "local/mistral"} {
		if !ids[id] {
			t.Fatalf("missing model id %q in %#v; body=%s", id, ids, body)
		}
	}
}

type burstyConfig struct {
	localURL            string
	trAPIKey            string
	trBaseURL           string
	trCatalogURL        string
	localMaxConcurrency int
	localQueueWait      time.Duration
	burstOnError        *bool
	token               string
	aliases             []string
	burstFallbackModel  string
	savingsReference    string
	stateFile           string
	cloud               string
	maxCloudSpend       string
}

type burstyProcess struct {
	baseURL  string
	client   *http.Client
	cmd      *exec.Cmd
	stderr   *bytes.Buffer
	exited   chan error
	stopOnce sync.Once
}

func startBursty(t *testing.T, cfg burstyConfig) *burstyProcess {
	t.Helper()
	addr := freeAddr(t)
	maxConcurrency := cfg.localMaxConcurrency
	if maxConcurrency == 0 {
		maxConcurrency = 4
	}
	burstOnError := true
	if cfg.burstOnError != nil {
		burstOnError = *cfg.burstOnError
	}
	trBaseURL := cfg.trBaseURL
	if trBaseURL == "" {
		trBaseURL = "http://127.0.0.1:1/v1"
	}
	trCatalogURL := cfg.trCatalogURL
	if trCatalogURL == "" {
		trCatalogURL = "http://127.0.0.1:1/v1"
	}
	cloud := cfg.cloud
	if cloud == "" {
		cloud = "auto"
	}
	maxCloudSpend := cfg.maxCloudSpend
	if maxCloudSpend == "" {
		maxCloudSpend = "0"
	}

	args := []string{
		"-listen", addr,
		"-local-url", cfg.localURL,
		"-tr-api-key", cfg.trAPIKey,
		"-tr-base-url", trBaseURL,
		"-tr-catalog-url", trCatalogURL,
		"-local-max-concurrency", strconv.Itoa(maxConcurrency),
		"-local-queue-wait", cfg.localQueueWait.String(),
		"-burst-on-error=" + strconv.FormatBool(burstOnError),
		"-token", cfg.token,
		"-burst-fallback-model", cfg.burstFallbackModel,
		"-savings-reference", cfg.savingsReference,
		"-state-file", cfg.stateFile,
		"-cloud", cloud,
		"-max-cloud-spend", maxCloudSpend,
	}
	for _, alias := range cfg.aliases {
		args = append(args, "-alias", alias)
	}
	cmd := exec.Command(burstyBinary, args...)
	cmd.Dir = repoRoot
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	exited := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start burstyrouter: %v\nstderr:\n%s", err, stderr.String())
	}
	go func() {
		exited <- cmd.Wait()
	}()

	proc := &burstyProcess{
		baseURL: "http://" + addr,
		client:  &http.Client{},
		cmd:     cmd,
		stderr:  stderr,
		exited:  exited,
	}
	t.Cleanup(func() {
		proc.stop(t)
		if t.Failed() {
			t.Logf("burstyrouter stderr:\n%s", proc.stderr.String())
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, proc.baseURL+"/healthz", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := proc.client.Do(req)
		cancel()
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return proc
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("burstyrouter did not become ready\nstderr:\n%s", stderr.String())
	return nil
}

func (p *burstyProcess) stop(t *testing.T) {
	t.Helper()
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-p.exited:
		case <-time.After(2 * time.Second):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			<-p.exited
		}
	})
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		skipIfLoopbackListenForbidden(t, err)
		t.Fatalf("listen on ephemeral port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		skipIfLoopbackListenForbidden(t, err)
		t.Fatalf("listen for test server: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = ln
	server.Start()
	return server
}

func skipIfLoopbackListenForbidden(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
		t.Skipf("loopback listen is not permitted in this sandbox: %v", err)
	}
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd, nil
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", fmt.Errorf("go.mod not found")
		}
		wd = parent
	}
}

func get(t *testing.T, proc *burstyProcess, path string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	return do(t, proc, http.MethodGet, path, "", headers)
}

func postChat(t *testing.T, proc *burstyProcess, body string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	return do(t, proc, http.MethodPost, chatPath, body, headers)
}

func postChatWithTimeout(t *testing.T, proc *burstyProcess, body string, headers http.Header, timeout time.Duration) (*http.Response, []byte) {
	t.Helper()
	return doWithTimeout(t, proc, http.MethodPost, chatPath, body, headers, timeout)
}

func do(t *testing.T, proc *burstyProcess, method, path, body string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	return doWithTimeout(t, proc, method, path, body, headers, 5*time.Second)
}

func doWithTimeout(t *testing.T, proc *burstyProcess, method, path, body string, headers http.Header, timeout time.Duration) (*http.Response, []byte) {
	t.Helper()
	resp, data, err := doResult(proc, method, path, body, headers, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func postChatResult(proc *burstyProcess, body string, headers http.Header, timeout time.Duration) (*http.Response, []byte, error) {
	return doResult(proc, http.MethodPost, chatPath, body, headers, timeout)
}

func doResult(proc *burstyProcess, method, path, body string, headers http.Header, timeout time.Duration) (*http.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, proc.baseURL+path, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("new request: %w", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	addHeaders(req.Header, headers)
	resp, err := proc.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, data, nil
}

func openChat(t *testing.T, proc *burstyProcess, body string, headers http.Header) (*http.Response, error) {
	t.Helper()
	return openChatWithTimeout(t, proc, body, headers, 5*time.Second)
}

func openChatWithTimeout(t *testing.T, proc *burstyProcess, body string, headers http.Header, timeout time.Duration) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proc.baseURL+chatPath, strings.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	addHeaders(req.Header, headers)
	resp, err := proc.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnCloseReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func addHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func xAPIKey(token string) http.Header {
	return http.Header{"X-Api-Key": {token}}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func assertStatsShape(t *testing.T, body []byte) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, body)
	}
	for _, key := range []string{"in_flight_local", "bursts_full", "bursts_error", "bursts_skipped_unmapped", "forced_local", "forced_tr", "requests_total", "catalog_errors", "cloud_blocked_budget", "cloud_blocked_mode", "cloud_mode", "savings", "routes", "endpoint_routes"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("stats missing key %q: %#v", key, payload)
		}
	}
	routes, ok := payload["routes"].(map[string]any)
	if !ok {
		t.Fatalf("stats routes has type %T", payload["routes"])
	}
	if _, ok := routes["local"]; !ok {
		t.Fatalf("stats routes missing local: %#v", routes)
	}
	if _, ok := routes["trustedrouter"]; !ok {
		t.Fatalf("stats routes missing trustedrouter: %#v", routes)
	}
	endpointRoutes, ok := payload["endpoint_routes"].(map[string]any)
	if !ok {
		t.Fatalf("stats endpoint_routes has type %T", payload["endpoint_routes"])
	}
	for _, key := range []string{"chat_completions", "embeddings", "messages", "responses"} {
		if _, ok := endpointRoutes[key]; !ok {
			t.Fatalf("stats endpoint_routes missing %q: %#v", key, endpointRoutes)
		}
	}
}

func assertRoute(t *testing.T, resp *http.Response, route, reason string) {
	t.Helper()
	if got := resp.Header.Get("X-Bursty-Route"); got != route {
		t.Fatalf("X-Bursty-Route = %q, want %q", got, route)
	}
	if got := resp.Header.Get("X-Bursty-Reason"); got != reason {
		t.Fatalf("X-Bursty-Reason = %q, want %q", got, reason)
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

func assertTopLevelString(t *testing.T, body []byte, key, want string) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("JSON body: %v\n%s", err, body)
	}
	if got, _ := payload[key].(string); got != want {
		t.Fatalf("top-level %q = %q, want %q in %s", key, got, want, body)
	}
}

func assertTopLevelBool(t *testing.T, body []byte, objectKey, boolKey string, want bool) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("JSON body: %v\n%s", err, body)
	}
	rawObject, ok := payload[objectKey]
	if !ok {
		t.Fatalf("missing top-level object %q in %s", objectKey, body)
	}
	var object map[string]bool
	if err := json.Unmarshal(rawObject, &object); err != nil {
		t.Fatalf("%s object JSON: %v\n%s", objectKey, err, body)
	}
	if got := object[boolKey]; got != want {
		t.Fatalf("%s.%s = %v, want %v in %s", objectKey, boolKey, got, want, body)
	}
}

func assertNoTopLevelKey(t *testing.T, body []byte, key string) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("JSON body: %v\n%s", err, body)
	}
	if _, ok := payload[key]; ok {
		t.Fatalf("top-level %q was forwarded in %s", key, body)
	}
}

func savedUSD(t *testing.T, body []byte) float64 {
	t.Helper()
	var payload struct {
		Savings struct {
			SavedUSD float64 `json:"saved_usd"`
		} `json:"savings"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, body)
	}
	return payload.Savings.SavedUSD
}

func assertSanitizedLocalHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, key := range []string{"Authorization", "Cookie", "X-Api-Key", "X-TrustedRouter-Workspace"} {
		if got := header.Get(key); got != "" {
			t.Fatalf("%s reached local: %q", key, got)
		}
	}
}

func assertSanitizedTrustedRouterHeaders(t *testing.T, header http.Header, apiKey string) {
	t.Helper()
	if got, want := header.Get("Authorization"), "Bearer "+apiKey; got != want {
		t.Fatalf("TrustedRouter Authorization = %q, want SDK bearer %q", got, want)
	}
	for _, key := range []string{"Cookie", "X-Api-Key", "X-TrustedRouter-Workspace"} {
		if got := header.Get(key); got != "" {
			t.Fatalf("%s reached TrustedRouter: %q", key, got)
		}
	}
}

func readSSEEventWithin(t *testing.T, reader *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	type result struct {
		event string
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		event, err := readSSEEvent(reader)
		ch <- result{event: event, err: err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read SSE event: %v", got.err)
		}
		return got.event
	case <-time.After(timeout):
		t.Fatalf("timed out reading SSE event after %s", timeout)
		return ""
	}
}

func readSSEEvent(reader *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		if line == "\n" {
			return b.String(), nil
		}
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
		t.Fatalf("models JSON: %v\n%s", err, body)
	}
	out := make(map[string]bool, len(payload.Data))
	for _, model := range payload.Data {
		if strings.HasPrefix(model.ID, "local/") && model.OwnedBy != "local" {
			t.Fatalf("local model %q owned_by = %q", model.ID, model.OwnedBy)
		}
		out[model.ID] = true
	}
	return out
}

type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type requestLog struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (l *requestLog) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

func (l *requestLog) last(t *testing.T) recordedRequest {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.requests) == 0 {
		t.Fatal("no recorded requests")
	}
	req := l.requests[len(l.requests)-1]
	req.Header = req.Header.Clone()
	req.Body = append([]byte(nil), req.Body...)
	return req
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r cancelOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
}
