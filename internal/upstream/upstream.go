package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	trustedrouter "github.com/Lore-Hex/trusted-router-go"
)

// localDialTimeout bounds dead local-rig SYN attempts so saturated local routes
// free Bursty slots quickly instead of waiting on OS TCP defaults.
const localDialTimeout = 3 * time.Second

// Local is an OpenAI-compatible local upstream.
type Local struct {
	baseURL string
	client  *http.Client
}

// NewLocal constructs a local upstream with a pooled HTTP transport.
func NewLocal(rawBaseURL string) (*Local, error) {
	return NewLocalWithHTTPClient(rawBaseURL, nil)
}

// NewLocalWithHTTPClient constructs a local upstream with a caller-supplied
// client. A nil client installs the production pooled transport.
func NewLocalWithHTTPClient(rawBaseURL string, client *http.Client) (*Local, error) {
	baseURL, err := NormalizeOpenAIBase(rawBaseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: localDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:          128,
				MaxIdleConnsPerHost:   64,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		}
	}
	return &Local{
		baseURL: baseURL,
		client:  client,
	}, nil
}

// NormalizeOpenAIBase returns a base URL that ends in /v1 exactly once.
func NormalizeOpenAIBase(rawBaseURL string) (string, error) {
	rawBaseURL = strings.TrimSpace(rawBaseURL)
	if rawBaseURL == "" {
		return "", fmt.Errorf("local base URL is empty")
	}
	parsed, err := url.Parse(rawBaseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("local base URL must include scheme and host")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/v1"
	} else if !strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path += "/v1"
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Chat forwards a chat completions request to local /v1/chat/completions.
func (l *Local) Chat(ctx context.Context, body []byte, inbound http.Header) (*http.Response, error) {
	return l.do(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(body), inbound)
}

// Models fetches local /v1/models.
func (l *Local) Models(ctx context.Context) (*http.Response, error) {
	return l.do(ctx, http.MethodGet, "/models", nil, nil)
}

func (l *Local) do(ctx context.Context, method, path string, body io.Reader, inbound http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	copyForwardHeaders(req.Header, inbound)
	if body != nil && req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}
	return l.client.Do(req)
}

// TrustedRouter is an SDK-backed TrustedRouter upstream.
type TrustedRouter struct {
	client *trustedrouter.Client
}

// NewTrustedRouter constructs a TrustedRouter upstream.
func NewTrustedRouter(apiKey, baseURL string) (*TrustedRouter, error) {
	return NewTrustedRouterWithHTTPClient(apiKey, baseURL, nil)
}

// NewTrustedRouterWithHTTPClient constructs a TrustedRouter upstream with a
// caller-supplied client. A nil client lets the SDK use its default client.
func NewTrustedRouterWithHTTPClient(apiKey, baseURL string, httpClient *http.Client) (*TrustedRouter, error) {
	// BurstyRouter is a proxy: inbound clients own retries, and SSE streams
	// must live as long as the inbound request context waits.
	client, err := trustedrouter.NewClient(trustedrouter.Options{
		APIKey:     apiKey,
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		MaxRetries: ptr(0),
		Timeout:    ptr(time.Duration(0)),
	})
	if err != nil {
		return nil, err
	}
	return &TrustedRouter{client: client}, nil
}

// Chat forwards a verbatim JSON request body with SDK auth. Retries and SDK
// timeouts stay disabled for proxy semantics; lifecycle is the inbound context.
func (t *TrustedRouter) Chat(ctx context.Context, body []byte, inbound http.Header) (*http.Response, error) {
	opts := &trustedrouter.CallOptions{
		ExtraHeaders: sanitizedExtraHeaders(inbound),
		Timeout:      ptr(time.Duration(0)),
	}
	return t.client.RawRequest(ctx, http.MethodPost, "/chat/completions", json.RawMessage(body), opts)
}

// Models fetches the TrustedRouter model catalog through the SDK.
func (t *TrustedRouter) Models(ctx context.Context) (*trustedrouter.ModelList, error) {
	return t.client.Models(ctx, nil)
}

func copyForwardHeaders(dst http.Header, src http.Header) {
	for key, value := range sanitizedExtraHeaders(src) {
		dst.Set(key, value)
	}
}

func sanitizedExtraHeaders(src http.Header) map[string]string {
	if src == nil {
		return nil
	}
	dynamicHopByHop := connectionHeaderTokens(src)
	out := make(map[string]string, len(src))
	for key, values := range src {
		if len(values) == 0 || shouldDropForwardHeader(key, dynamicHopByHop) {
			continue
		}
		out[key] = strings.Join(values, ", ")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shouldDropForwardHeader(key string, dynamicHopByHop map[string]struct{}) bool {
	lower := strings.ToLower(key)
	if _, ok := dynamicHopByHop[lower]; ok {
		return true
	}
	if strings.HasPrefix(lower, "x-trustedrouter-") {
		return true
	}
	switch lower {
	case "accept-encoding", "authorization", "connection", "content-length", "cookie", "host",
		"keep-alive", "proxy-authenticate", "proxy-authorization", "set-cookie", "te",
		"trailer", "transfer-encoding", "upgrade", "x-api-key":
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

func ptr[T any](value T) *T {
	return &value
}
