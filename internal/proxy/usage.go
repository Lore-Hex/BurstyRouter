package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxSSELineBytes = 1 << 20

type usageCapture struct {
	Usage    tokenUsage
	Model    string
	HasUsage bool
}

func extractUsageAndModel(body []byte) usageCapture {
	var payload struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Usage == nil {
		return usageCapture{Model: payload.Model}
	}
	promptTokens := payload.Usage.PromptTokens
	if promptTokens == 0 {
		promptTokens = payload.Usage.InputTokens
	}
	completionTokens := payload.Usage.CompletionTokens
	if completionTokens == 0 {
		completionTokens = payload.Usage.OutputTokens
	}
	return usageCapture{
		Usage: tokenUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
		},
		Model:    payload.Model,
		HasUsage: true,
	}
}

type streamUsageScanner struct {
	line    []byte
	seen    usageCapture
	discard bool
}

func (s *streamUsageScanner) Feed(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			return
		}
		s.appendLine(p[:i+1])
		s.processLine(s.line)
		s.line = s.line[:0]
		s.discard = false
		p = p[i+1:]
	}
}

func (s *streamUsageScanner) Finish() usageCapture {
	if len(s.line) > 0 && !s.discard {
		s.processLine(s.line)
	}
	return s.seen
}

func (s *streamUsageScanner) appendLine(p []byte) {
	if s.discard {
		return
	}
	if len(s.line)+len(p) > maxSSELineBytes {
		s.line = s.line[:0]
		s.discard = true
		return
	}
	s.line = append(s.line, p...)
}

func (s *streamUsageScanner) processLine(line []byte) {
	line = bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	capture := extractUsageAndModel(data)
	// Latch only positive counts so a trailing usage:{0,0} chunk cannot zero out
	// the true totals captured from the last content chunk.
	if capture.HasUsage && (capture.Usage.PromptTokens > 0 || capture.Usage.CompletionTokens > 0) {
		s.seen = capture
	}
}

type usageScanningWriter struct {
	dst     io.Writer
	scanner *streamUsageScanner
}

func (w usageScanningWriter) Write(p []byte) (int, error) {
	if w.scanner != nil {
		w.scanner.Feed(p)
	}
	return w.dst.Write(p)
}

func responseModel(capture usageCapture, fallback string) string {
	if strings.TrimSpace(capture.Model) != "" {
		return capture.Model
	}
	return fallback
}

func shouldCaptureUsage(resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
