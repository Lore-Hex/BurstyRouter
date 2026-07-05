package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	trustedrouter "github.com/Lore-Hex/trusted-router-go"
)

func TestNormalizeOpenAIBase(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"http://127.0.0.1:11434":     "http://127.0.0.1:11434/v1",
		"http://127.0.0.1:11434/":    "http://127.0.0.1:11434/v1",
		"http://127.0.0.1:11434/v1":  "http://127.0.0.1:11434/v1",
		"http://127.0.0.1:11434/v1/": "http://127.0.0.1:11434/v1",
		"http://host/base":           "http://host/base/v1",
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeOpenAIBase(raw)
			if err != nil {
				t.Fatalf("NormalizeOpenAIBase() error = %v", err)
			}
			if got != want {
				t.Fatalf("NormalizeOpenAIBase() = %q, want %q", got, want)
			}
		})
	}
}

func TestSanitizedExtraHeadersJoinsMultipleValues(t *testing.T) {
	t.Parallel()

	got := sanitizedExtraHeaders(http.Header{
		"Accept":                    {"application/json", "text/event-stream"},
		"Accept-Encoding":           {"gzip"},
		"Authorization":             {"Bearer inbound"},
		"Connection":                {"X-Hop, keep-alive"},
		"Cookie":                    {"session=secret"},
		"Set-Cookie":                {"session=secret"},
		"X-Api-Key":                 {"secret"},
		"X-Hop":                     {"secret"},
		"X-TrustedRouter-Workspace": {"smuggled"},
	})
	if got["Accept"] != "application/json, text/event-stream" {
		t.Fatalf("Accept = %q, want joined values", got["Accept"])
	}
	for _, key := range []string{"Accept-Encoding", "Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Hop", "X-TrustedRouter-Workspace"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s forwarded: %#v", key, got)
		}
	}
}

func TestTrustedRouterDisablesSDKTimeoutForProxyStreams(t *testing.T) {
	const first = "data: one\n\n"
	const second = "data: two\n\n"

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: &delayedContextBody{
				ctx:    r.Context(),
				chunks: [][]byte{[]byte(first), []byte(second)},
				delay:  200 * time.Millisecond,
			},
			Request: r,
		}, nil
	})
	httpClient := &http.Client{Transport: transport}

	timeout := 50 * time.Millisecond
	control, err := trustedrouter.NewClient(trustedrouter.Options{
		APIKey:     "tr-key",
		BaseURL:    "http://tr.test/v1",
		HTTPClient: httpClient,
		MaxRetries: ptr(0),
		Timeout:    &timeout,
	})
	if err != nil {
		t.Fatalf("control NewClient() error = %v", err)
	}
	controlResp, err := control.RawRequest(context.Background(), http.MethodPost, "/chat/completions", json.RawMessage(`{"model":"x"}`), nil)
	if err != nil {
		t.Fatalf("control RawRequest() error before body read = %v", err)
	}
	_, err = io.ReadAll(controlResp.Body)
	_ = controlResp.Body.Close()
	if err == nil {
		t.Fatal("control body read succeeded, want SDK timeout to prove the mechanism")
	}

	production, err := NewTrustedRouterWithHTTPClient("tr-key", "http://tr.test/v1", httpClient)
	if err != nil {
		t.Fatalf("NewTrustedRouterWithHTTPClient() error = %v", err)
	}
	resp, err := production.Chat(context.Background(), []byte(`{"model":"x"}`), nil)
	if err != nil {
		t.Fatalf("production Chat() error = %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("production body read error = %v", err)
	}
	if string(got) != first+second {
		t.Fatalf("stream body = %q, want %q", got, first+second)
	}
}

func TestTrustedRouterDoesNotRetryProxyRequests(t *testing.T) {
	var posts atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			posts.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    r,
		}, nil
	})

	tr, err := NewTrustedRouterWithHTTPClient("tr-key", "http://tr.test/v1", &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewTrustedRouterWithHTTPClient() error = %v", err)
	}
	resp, err := tr.Chat(context.Background(), []byte(`{"model":"x"}`), nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	_ = resp.Body.Close()
	if posts.Load() != 1 {
		t.Fatalf("POSTs = %d, want exactly 1", posts.Load())
	}
}

func TestTrustedRouterRawRequestBodyIsVerbatim(t *testing.T) {
	body := []byte("{\n  \"model\": \"trustedrouter/auto\",\n  \"messages\": [\n    {\"role\":\"user\",\"content\":\"<b> & \u2028\"}\n  ]\n}\n")
	seen := make(chan []byte, 1)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got, _ := io.ReadAll(r.Body)
		seen <- got
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"tr"}`)),
			Request:    r,
		}, nil
	})

	tr, err := NewTrustedRouterWithHTTPClient("tr-key", "http://tr.test/v1", &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("NewTrustedRouterWithHTTPClient() error = %v", err)
	}
	resp, err := tr.Chat(context.Background(), body, nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	_ = resp.Body.Close()
	if got := <-seen; !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want byte-identical %q", got, body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type delayedContextBody struct {
	ctx    context.Context
	chunks [][]byte
	delay  time.Duration
	index  int
}

func (b *delayedContextBody) Read(p []byte) (int, error) {
	if b.index >= len(b.chunks) {
		return 0, io.EOF
	}
	if b.index > 0 {
		select {
		case <-time.After(b.delay):
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	chunk := b.chunks[b.index]
	b.index++
	return copy(p, chunk), nil
}

func (b *delayedContextBody) Close() error {
	return nil
}
