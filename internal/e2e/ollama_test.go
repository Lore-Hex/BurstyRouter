package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealOllamaLocalModel(t *testing.T) {
	ollamaURL := strings.TrimRight(strings.TrimSpace(os.Getenv("BURSTY_E2E_OLLAMA_URL")), "/")
	if ollamaURL == "" {
		t.Skip("BURSTY_E2E_OLLAMA_URL is unset")
	}
	model := firstOllamaModel(t, ollamaURL)

	proc := startBursty(t, burstyConfig{localURL: ollamaURL})

	chatBody := mustJSON(t, map[string]any{
		"model":      model,
		"max_tokens": 8,
		"stream":     false,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with one short word."},
		},
	})
	resp, body := postChatWithTimeout(t, proc, chatBody, nil, 90*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-streaming status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "local", "policy")
	assertBurstyBlock(t, body, "local", "policy")
	var completion struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		t.Fatalf("completion JSON: %v\n%s", err, body)
	}
	if len(completion.Choices) == 0 {
		t.Fatalf("completion has no choices: %s", body)
	}
	if _, ok := completion.Choices[0].Message.Content.(string); !ok {
		t.Fatalf("choices[0].message.content has type %T, want string; body=%s", completion.Choices[0].Message.Content, body)
	}

	streamBody := mustJSON(t, map[string]any{
		"model":      model,
		"max_tokens": 8,
		"stream":     true,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with one short word."},
		},
	})
	streamResp, err := openChatWithTimeout(t, proc, streamBody, nil, 90*time.Second)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("streaming status = %d body=%s", streamResp.StatusCode, data)
	}
	assertRoute(t, streamResp, "local", "policy")
	sawDelta, sawDone := readOpenAIStream(t, bufio.NewReader(streamResp.Body), 90*time.Second)
	if !sawDelta {
		t.Fatal("stream did not include a chunk with choices[0].delta")
	}
	if !sawDone {
		t.Fatal("stream did not include terminal [DONE]")
	}

	resp, body = get(t, proc, "/v1/models", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d body=%s", resp.StatusCode, body)
	}
	assertRoute(t, resp, "local", "policy")
	ids := modelIDs(t, body)
	for _, id := range []string{model, "local/" + model} {
		if !ids[id] {
			t.Fatalf("missing model id %q in %#v; body=%s", id, ids, body)
		}
	}
}

func firstOllamaModel(t *testing.T, ollamaURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaURL+"/api/tags", nil)
	if err != nil {
		t.Fatalf("new Ollama tags request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("Ollama probe failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		t.Skipf("Ollama probe status = %d body=%s", resp.StatusCode, data)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		t.Skipf("Ollama tags JSON: %v", err)
	}
	for _, model := range tags.Models {
		if strings.TrimSpace(model.Name) != "" {
			return model.Name
		}
	}
	t.Skip("Ollama is reachable but has no local models")
	return ""
}

func readOpenAIStream(t *testing.T, reader *bufio.Reader, timeout time.Duration) (bool, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	sawDelta := false
	for time.Now().Before(deadline) {
		event := readSSEEventWithin(t, reader, time.Until(deadline))
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				return sawDelta, true
			}
			var chunk struct {
				Choices []struct {
					Delta map[string]any `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
				sawDelta = true
			}
		}
	}
	return sawDelta, false
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(data)
}
