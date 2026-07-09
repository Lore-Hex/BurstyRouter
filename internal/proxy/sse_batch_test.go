package proxy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func contentChunk(content string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":` +
		strconv.Quote(content) + `},"finish_reason":null}]}` + "\n\n"
}

func roleChunk() string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"
}

func finishChunk() string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
}

func usageChunk() string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5}}` + "\n\n"
}

// contentChunkRole mirrors ollama's shape: role repeated in every delta.
func contentChunkRole(content string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":` +
		strconv.Quote(content) + `},"finish_reason":null}]}` + "\n\n"
}

// reasoningChunk carries reasoning text (ollama's thinking phase); it must not
// be merged, or the thinking trace would be dropped.
func reasoningChunk(reasoning string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning":` +
		strconv.Quote(reasoning) + `},"finish_reason":null}]}` + "\n\n"
}

const doneEvent = "data: [DONE]\n\n"

// eventContents returns, per SSE event, the chat.completion.chunk delta content
// (or "[DONE]" / "<raw>" for non-content events), in order.
func eventContents(t *testing.T, out string) []string {
	t.Helper()
	var contents []string
	for _, frame := range strings.Split(out, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(frame, "data:"))
		if payload == "[DONE]" {
			contents = append(contents, "[DONE]")
			continue
		}
		var obj struct {
			Object  string    `json:"object"`
			Usage   *struct{} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
					Role    *string `json:"role"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("event is not valid JSON: %v\n%s", err, payload)
		}
		switch {
		case obj.Usage != nil:
			contents = append(contents, "<usage>")
		case len(obj.Choices) == 1 && obj.Choices[0].Delta.Content != nil:
			contents = append(contents, *obj.Choices[0].Delta.Content)
		default:
			contents = append(contents, "<other>")
		}
	}
	return contents
}

func runCoalescer(t *testing.T, window time.Duration, maxBytes int, writes ...string) string {
	t.Helper()
	var out safeBuffer
	c := newSSEBatchWriter(&out, window, maxBytes)
	for _, chunk := range writes {
		if _, err := c.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return out.String()
}

func TestSSEBatchDisabledIsTransparent(t *testing.T) {
	t.Parallel()
	in := contentChunk("A") + contentChunk("B") + contentChunk("C") + doneEvent
	out := runCoalescer(t, 0, 4096, in)
	if out != in {
		t.Fatalf("disabled coalescer altered the stream:\n got %q\nwant %q", out, in)
	}
}

func TestSSEBatchMergesConsecutiveContent(t *testing.T) {
	t.Parallel()
	// First content passes immediately; the rest merge into one event; [DONE]
	// flushes the merge and passes through.
	out := runCoalescer(t, 50*time.Millisecond, 4096,
		contentChunk("A"), contentChunk("B"), contentChunk("C"), doneEvent)
	got := eventContents(t, out)
	want := []string{"A", "BC", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if reassembled := strings.Join(nonMarkerContents(got), ""); reassembled != "ABC" {
		t.Fatalf("reassembled content = %q, want ABC", reassembled)
	}
}

func TestSSEBatchMergesOllamaRoleDeltas(t *testing.T) {
	t.Parallel()
	// ollama repeats role in every delta; those must still coalesce.
	out := runCoalescer(t, 50*time.Millisecond, 4096,
		contentChunkRole("A"), contentChunkRole("B"), contentChunkRole("C"), doneEvent)
	got := eventContents(t, out)
	want := []string{"A", "BC", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	// The merged chunk must keep role:"assistant".
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("merged chunk dropped role:\n%s", out)
	}
}

func TestSSEBatchNeverMergesReasoningDeltas(t *testing.T) {
	t.Parallel()
	// Reasoning deltas carry a reasoning field; merging them as content would
	// drop the thinking trace. They must pass through byte-for-byte.
	out := runCoalescer(t, 50*time.Millisecond, 4096,
		reasoningChunk("think one"), reasoningChunk("think two"), contentChunkRole("answer"), doneEvent)
	if !strings.Contains(out, "think one") || !strings.Contains(out, "think two") {
		t.Fatalf("a reasoning trace was dropped:\n%s", out)
	}
	// Both reasoning frames must survive as separate events (not merged away).
	if n := strings.Count(out, `"reasoning":`); n != 2 {
		t.Fatalf("reasoning frame count = %d, want 2\n%s", n, out)
	}
}

func TestSSEBatchNonMergeableFlushesAndPassesThrough(t *testing.T) {
	t.Parallel()
	// role delta and finish chunk are non-mergeable; the buffered B is flushed
	// verbatim before each.
	out := runCoalescer(t, 50*time.Millisecond, 4096,
		roleChunk(), contentChunk("A"), contentChunk("B"), finishChunk(), doneEvent)
	got := eventContents(t, out)
	want := []string{"<other>", "A", "B", "<other>", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestSSEBatchUsageChunkSurvivesIntact(t *testing.T) {
	t.Parallel()
	// The usage chunk must pass through unmerged so the savings scanner sees it.
	out := runCoalescer(t, 50*time.Millisecond, 4096,
		contentChunk("A"), contentChunk("B"), usageChunk(), doneEvent)
	got := eventContents(t, out)
	want := []string{"A", "B", "<usage>", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if !strings.Contains(out, `"usage":{"prompt_tokens":3,"completion_tokens":5}`) {
		t.Fatalf("usage chunk was altered:\n%s", out)
	}
}

func TestSSEBatchMaxBytesForcesFlush(t *testing.T) {
	t.Parallel()
	out := runCoalescer(t, time.Second, 3,
		contentChunk("AA"), contentChunk("BB"), contentChunk("CC"), contentChunk("DD"), doneEvent)
	got := eventContents(t, out)
	// AA immediate; BB buffered; CC merge pushes content to 4 > 3 -> flush "BBCC";
	// DD buffered, flushed on [DONE].
	want := []string{"AA", "BBCC", "DD", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if reassembled := strings.Join(nonMarkerContents(got), ""); reassembled != "AABBCCDD" {
		t.Fatalf("reassembled content = %q, want AABBCCDD", reassembled)
	}
}

func TestSSEBatchReassemblesPartialWrites(t *testing.T) {
	t.Parallel()
	a := contentChunk("A")
	b := contentChunk("B")
	// Split the first event across two writes at an arbitrary byte boundary.
	out := runCoalescer(t, 50*time.Millisecond, 4096, a[:len(a)/2], a[len(a)/2:]+b, doneEvent)
	got := eventContents(t, out)
	want := []string{"A", "B", "[DONE]"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestSSEBatchTimerFlushesWithoutClose(t *testing.T) {
	t.Parallel()
	var out safeBuffer
	c := newSSEBatchWriter(&out, 20*time.Millisecond, 4096)
	// A passes immediately; B is buffered and should be flushed by the timer.
	if _, err := c.Write([]byte(contentChunk("A") + contentChunk("B"))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(eventContents(t, out.String())) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := eventContents(t, out.String())
	if strings.Join(got, "|") != "A|B" {
		t.Fatalf("after timer, events = %v, want [A B]", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func nonMarkerContents(events []string) []string {
	var out []string
	for _, e := range events {
		if e == "[DONE]" || strings.HasPrefix(e, "<") {
			continue
		}
		out = append(out, e)
	}
	return out
}
