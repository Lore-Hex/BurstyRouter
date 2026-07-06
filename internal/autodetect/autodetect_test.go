package autodetect

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDetectFound(t *testing.T) {
	t.Parallel()

	client := probeClient(map[string]probeResponse{
		"local.test": {status: http.StatusOK, body: `{"object":"list","data":[{"id":"a"},{"id":"b"}]}`},
	})
	result, ok := Detect(context.Background(), []Probe{{Name: "test", URL: "http://local.test"}}, 20*time.Millisecond, client)
	if !ok {
		t.Fatal("Detect() ok = false, want true")
	}
	if result.URL != "http://local.test/v1" || result.Name != "test" || result.ModelCount != 2 {
		t.Fatalf("Detect() = %#v", result)
	}
}

func TestDetectNotFound(t *testing.T) {
	t.Parallel()

	client := probeClient(map[string]probeResponse{
		"local.test": {status: http.StatusNotFound, body: `{"error":"missing"}`},
	})
	if result, ok := Detect(context.Background(), []Probe{{Name: "test", URL: "http://local.test"}}, 20*time.Millisecond, client); ok {
		t.Fatalf("Detect() = %#v, true; want no result", result)
	}
}

func TestDetectOrdering(t *testing.T) {
	t.Parallel()

	client := probeClient(map[string]probeResponse{
		"first.test":  {status: http.StatusBadGateway, body: `{"error":"down"}`},
		"second.test": {status: http.StatusOK, body: `{"data":[{"id":"second"}]}`},
		"third.test":  {status: http.StatusOK, body: `{"data":[{"id":"third"}]}`},
	})

	result, ok := Detect(context.Background(), []Probe{
		{Name: "first", URL: "http://first.test"},
		{Name: "second", URL: "http://second.test"},
		{Name: "third", URL: "http://third.test"},
	}, 20*time.Millisecond, client)
	if !ok {
		t.Fatal("Detect() ok = false, want true")
	}
	if result.Name != "second" || result.ModelCount != 1 {
		t.Fatalf("Detect() = %#v, want second", result)
	}
}

func TestDetectTimeout(t *testing.T) {
	t.Parallel()

	client := probeClient(map[string]probeResponse{
		"slow.test": {delay: 50 * time.Millisecond, status: http.StatusOK, body: `{"data":[{"id":"slow"}]}`},
		"fast.test": {status: http.StatusOK, body: `{"data":[{"id":"fast"},{"id":"other"}]}`},
	})

	start := time.Now()
	result, ok := Detect(context.Background(), []Probe{
		{Name: "slow", URL: "http://slow.test"},
		{Name: "fast", URL: "http://fast.test"},
	}, 10*time.Millisecond, client)
	if !ok {
		t.Fatal("Detect() ok = false, want true")
	}
	if result.Name != "fast" || result.ModelCount != 2 {
		t.Fatalf("Detect() = %#v, want fast", result)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Detect() took %s, want timeout to advance to next probe", elapsed)
	}
}

func TestDefaultProbesIncludesOllamaHostFirst(t *testing.T) {
	t.Parallel()

	probes := DefaultProbes(func(key string) (string, bool) {
		if key == "OLLAMA_HOST" {
			return "example.test:11434", true
		}
		return "", false
	})
	if len(probes) == 0 || probes[0].URL != "example.test:11434" || probes[0].Name != "Ollama" {
		t.Fatalf("DefaultProbes()[0] = %#v", probes[0])
	}
}

func TestNormalizeBase(t *testing.T) {
	t.Parallel()

	got, err := NormalizeBase("127.0.0.1:11434")
	if err != nil {
		t.Fatalf("NormalizeBase() error = %v", err)
	}
	if got != "http://127.0.0.1:11434/v1" {
		t.Fatalf("NormalizeBase() = %q", got)
	}
}

type probeResponse struct {
	delay  time.Duration
	status int
	body   string
}

func probeClient(responses map[string]probeResponse) *http.Client {
	return &http.Client{Transport: probeTransport(responses)}
}

type probeTransport map[string]probeResponse

func (t probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, ok := t[req.URL.Host]
	if !ok || req.URL.Path != "/v1/models" {
		return responseFromRecorder(req, http.StatusNotFound, `{"error":"missing"}`), nil
	}
	if response.delay > 0 {
		select {
		case <-time.After(response.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	return responseFromRecorder(req, response.status, response.body), nil
}

func responseFromRecorder(req *http.Request, status int, body string) *http.Response {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/json")
	recorder.WriteHeader(status)
	_, _ = io.Copy(recorder, strings.NewReader(body))
	resp := recorder.Result()
	resp.Request = req
	return resp
}
