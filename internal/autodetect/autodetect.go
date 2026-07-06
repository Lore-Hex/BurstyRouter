package autodetect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Lore-Hex/BurstyRouter/internal/upstream"
)

// DefaultProbeTimeout is the per-candidate local server probe budget.
const DefaultProbeTimeout = 750 * time.Millisecond

// Probe is one local server candidate to test.
type Probe struct {
	Name string
	URL  string
}

// Result describes a responsive local OpenAI-compatible server.
type Result struct {
	Name       string
	URL        string
	ModelCount int
}

// DefaultProbes returns local server candidates in startup preference order.
func DefaultProbes(lookupEnv func(string) (string, bool)) []Probe {
	var probes []Probe
	if value, ok := lookupEnv("OLLAMA_HOST"); ok && strings.TrimSpace(value) != "" {
		probes = append(probes, Probe{Name: "Ollama", URL: value})
	}
	probes = append(probes,
		Probe{Name: "Ollama", URL: "127.0.0.1:11434"},
		Probe{Name: "LM Studio", URL: "127.0.0.1:1234"},
		Probe{Name: "llama.cpp", URL: "127.0.0.1:8080"},
		Probe{Name: "vLLM", URL: "127.0.0.1:8000"},
	)
	return probes
}

// Detect probes candidates in order and returns the first responsive server.
func Detect(ctx context.Context, probes []Probe, timeout time.Duration, client *http.Client) (Result, bool) {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	if client == nil {
		client = http.DefaultClient
	}
	for _, probe := range probes {
		result, err := ProbeServer(ctx, probe, timeout, client)
		if err == nil {
			return result, true
		}
	}
	return Result{}, false
}

// ProbeServer performs one GET /v1/models probe.
func ProbeServer(ctx context.Context, probe Probe, timeout time.Duration, client *http.Client) (Result, error) {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	if client == nil {
		client = http.DefaultClient
	}
	baseURL, err := NormalizeBase(probe.URL)
	if err != nil {
		return Result{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return Result{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("%s returned %s", baseURL, resp.Status)
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return Result{}, err
	}
	return Result{
		Name:       flavorName(probe.Name, baseURL),
		URL:        baseURL,
		ModelCount: len(payload.Data),
	}, nil
}

// NormalizeBase returns an OpenAI-compatible base URL ending in /v1.
func NormalizeBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("probe URL is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return upstream.NormalizeOpenAIBase(raw)
}

// GuessFlavor returns a best-effort local server flavor for display.
func GuessFlavor(raw string) string {
	baseURL, err := NormalizeBase(raw)
	if err != nil {
		return "OpenAI-compatible"
	}
	return flavorName("", baseURL)
}

func flavorName(name, baseURL string) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "OpenAI-compatible"
	}
	switch parsed.Port() {
	case "11434":
		return "Ollama"
	case "1234":
		return "LM Studio"
	case "8080":
		return "llama.cpp"
	case "8000":
		return "vLLM"
	default:
		return "OpenAI-compatible"
	}
}
