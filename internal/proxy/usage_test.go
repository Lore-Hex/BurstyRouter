package proxy

import "testing"

func TestStreamUsageScannerIgnoresTrailingZeroUsage(t *testing.T) {
	t.Parallel()

	// A provider attaches real usage to its last content chunk, then sends a
	// spec-compliant trailing choices:[] chunk with usage:{0,0}. The true totals
	// must survive.
	frames := []string{
		`data: {"model":"m","choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":8,"completion_tokens":3}}` + "\n",
		`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}` + "\n",
		"data: [DONE]\n",
	}
	var scanner streamUsageScanner
	for _, frame := range frames {
		scanner.Feed([]byte(frame))
	}
	got := scanner.Finish()
	if !got.HasUsage {
		t.Fatalf("HasUsage = false, want true")
	}
	if got.Usage.PromptTokens != 8 || got.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want prompt 8 completion 3", got.Usage)
	}
	if got.Model != "m" {
		t.Fatalf("model = %q, want m", got.Model)
	}
}

func TestExtractUsageAndModelInputOutputFallback(t *testing.T) {
	t.Parallel()

	got := extractUsageAndModel([]byte(`{"model":"m","usage":{"input_tokens":11,"output_tokens":4}}`))
	if !got.HasUsage {
		t.Fatalf("HasUsage = false, want true")
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 4 {
		t.Fatalf("usage = %+v, want prompt 11 completion 4", got.Usage)
	}
}
