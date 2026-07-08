package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTranslateRequestGolden(t *testing.T) {
	raw := []byte(`{
		"model":"llama3",
		"anthropic_version":"2023-06-01",
		"metadata":{"user_id":"u"},
		"extra":"dropped",
		"system":[
			{"type":"text","text":"You are "},
			{"type":"text","text":"local","cache_control":{"type":"ephemeral"}}
		],
		"max_tokens":64,
		"temperature":0.2,
		"top_p":0.9,
		"stop_sequences":["END"],
		"stream":true,
		"tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"lookup"},
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[
				{"type":"text","text":""},
				{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"a"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"result"}],"is_error":true},
				{"type":"text","text":" thanks"}
			]}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest() error = %v", err)
	}
	want := []byte(`{
		"model":"llama3",
		"messages":[
			{"role":"system","content":"You are local"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"a\"}"}}]},
			{"role":"tool","content":"[error] result","tool_call_id":"toolu_1"},
			{"role":"user","content":" thanks"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"max_tokens":64,
		"temperature":0.2,
		"top_p":0.9,
		"stop":["END"],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`)
	assertJSONEqual(t, got, want)
}

func TestTranslateRequestForwardsTopKAndThinking(t *testing.T) {
	raw := []byte(`{
		"model":"qwen3",
		"max_tokens":100,
		"top_k":40,
		"thinking":{"type":"enabled","budget_tokens":2048},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest() error = %v", err)
	}
	want := []byte(`{
		"model":"qwen3",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":100,
		"top_k":40,
		"reasoning_effort":"medium",
		"stream":false
	}`)
	assertJSONEqual(t, got, want)
}

func TestReasoningEffortFromThinking(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{``, ""},
		{`null`, ""},
		{`{"type":"disabled"}`, ""},
		{`{"type":"enabled","budget_tokens":512}`, "low"},
		{`{"type":"enabled","budget_tokens":1024}`, "low"},
		{`{"type":"enabled","budget_tokens":2048}`, "medium"},
		{`{"type":"enabled","budget_tokens":4096}`, "medium"},
		{`{"type":"enabled","budget_tokens":16000}`, "high"},
		{`{"type":"enabled"}`, "medium"},
		{`{"budget_tokens":900}`, "low"},
	}
	for _, c := range cases {
		if got := reasoningEffortFromThinking([]byte(c.in)); got != c.want {
			t.Fatalf("reasoningEffortFromThinking(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTranslateContentFilterMapsToRefusal(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		raw := []byte(`{"model":"m","choices":[{"message":{"content":"blocked"},"finish_reason":"content_filter"}]}`)
		got, _, err := TranslateResponse(raw, "visible")
		if err != nil {
			t.Fatalf("TranslateResponse() error = %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(got, &out); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, got)
		}
		if out["stop_reason"] != "refusal" {
			t.Fatalf("stop_reason = %v, want refusal", out["stop_reason"])
		}
	})

	t.Run("stream", func(t *testing.T) {
		input := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"blocked"},"finish_reason":"content_filter"}]}`,
			`data: [DONE]`,
			``,
		}, "\n\n")
		var out bytes.Buffer
		if _, err := TranslateStream(strings.NewReader(input), &out, "visible"); err != nil {
			t.Fatalf("TranslateStream() error = %v\n%s", err, out.String())
		}
		if got := stopReason(t, parseSSEEvents(t, out.String())); got != "refusal" {
			t.Fatalf("stream stop_reason = %q, want refusal", got)
		}
	})
}

func TestTranslateResponseSurfacesReasoningAndCacheTokens(t *testing.T) {
	raw := []byte(`{
		"model":"m",
		"choices":[{"message":{"reasoning_content":"let me think","content":"answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}
	}`)
	got, usage, err := TranslateResponse(raw, "visible")
	if err != nil {
		t.Fatalf("TranslateResponse() error = %v", err)
	}
	if usage.CachedTokens != 4 {
		t.Fatalf("Usage.CachedTokens = %d, want 4", usage.CachedTokens)
	}
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		Usage struct {
			InputTokens          int64 `json:"input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, got)
	}
	if len(out.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (thinking, text)\n%s", len(out.Content), got)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "let me think" {
		t.Fatalf("block[0] = %+v, want thinking 'let me think'", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "answer" {
		t.Fatalf("block[1] = %+v, want text 'answer'", out.Content[1])
	}
	if out.Usage.CacheReadInputTokens != 4 {
		t.Fatalf("usage.cache_read_input_tokens = %d, want 4", out.Usage.CacheReadInputTokens)
	}
}

func TestTranslateStreamIgnoresTrailingZeroUsage(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":8,"completion_tokens":3}}`,
		`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var out bytes.Buffer
	usage, err := TranslateStream(strings.NewReader(input), &out, "visible")
	if err != nil {
		t.Fatalf("TranslateStream() error = %v\n%s", err, out.String())
	}
	if usage.PromptTokens != 8 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want prompt 8 completion 3", usage)
	}
}

func TestTranslateRequestToolChoiceAndArguments(t *testing.T) {
	tests := []struct {
		name          string
		toolChoice    string
		wantChoice    string
		toolUseInput1 string
		toolUseInput2 string
		wantArg1      string
		wantArg2      string
	}{
		{
			name:          "auto",
			toolChoice:    `{"type":"auto"}`,
			wantChoice:    `"auto"`,
			toolUseInput1: `null`,
			toolUseInput2: `"raw-json"`,
			wantArg1:      `{}`,
			wantArg2:      `raw-json`,
		},
		{
			name:          "any",
			toolChoice:    `{"type":"any"}`,
			wantChoice:    `"required"`,
			toolUseInput1: `{"n":1}`,
			toolUseInput2: `{"ok":true}`,
			wantArg1:      `{"n":1}`,
			wantArg2:      `{"ok":true}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{
				"model":"llama3",
				"max_tokens":8,
				"tool_choice":` + tt.toolChoice + `,
				"messages":[{"role":"assistant","content":[
					{"type":"tool_use","id":"a","name":"first","input":` + tt.toolUseInput1 + `},
					{"type":"tool_use","id":"b","name":"second","input":` + tt.toolUseInput2 + `}
				]}]
			}`)
			got, err := TranslateRequest(raw)
			if err != nil {
				t.Fatalf("TranslateRequest() error = %v", err)
			}
			var payload struct {
				ToolChoice json.RawMessage `json:"tool_choice"`
				Messages   []struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("translated JSON: %v\n%s", err, got)
			}
			assertJSONEqual(t, payload.ToolChoice, []byte(tt.wantChoice))
			if gotArg := payload.Messages[0].ToolCalls[0].Function.Arguments; gotArg != tt.wantArg1 {
				t.Fatalf("arg1 = %q, want %q in %s", gotArg, tt.wantArg1, got)
			}
			if gotArg := payload.Messages[0].ToolCalls[1].Function.Arguments; gotArg != tt.wantArg2 {
				t.Fatalf("arg2 = %q, want %q in %s", gotArg, tt.wantArg2, got)
			}
		})
	}
}

func TestTranslateRequestRejectsImages(t *testing.T) {
	_, err := TranslateRequest([]byte(`{
		"model":"llama3",
		"max_tokens":8,
		"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]
	}`))
	var anthropicErr *Error
	if !errors.As(err, &anthropicErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if anthropicErr.Status != 400 || anthropicErr.Type != "invalid_request_error" || anthropicErr.Message != imageUnsupportedMessage {
		t.Fatalf("anthropic error = %#v", anthropicErr)
	}
}

func TestTranslateResponseGolden(t *testing.T) {
	withFixedMessageID(t, "msg_test")

	raw := []byte(`{
		"model":"llama3",
		"choices":[{
			"message":{
				"content":"hello",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"a\"}"}}]
			},
			"finish_reason":"tool_calls"
		}],
		"usage":{"prompt_tokens":11,"completion_tokens":3}
	}`)
	got, usage, err := TranslateResponse(raw, "anthropic/claude-haiku-4.5")
	if err != nil {
		t.Fatalf("TranslateResponse() error = %v", err)
	}
	want := []byte(`{
		"id":"msg_test",
		"type":"message",
		"role":"assistant",
		"model":"anthropic/claude-haiku-4.5",
		"content":[
			{"type":"text","text":"hello"},
			{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"a"}}
		],
		"stop_reason":"tool_use",
		"stop_sequence":null,
		"usage":{"input_tokens":11,"output_tokens":3}
	}`)
	assertJSONEqual(t, got, want)
	if !usage.HasUsage || usage.PromptTokens != 11 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestTranslateStreamGolden(t *testing.T) {
	withFixedMessageID(t, "msg_test")

	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello "}}]}`,
		`data: {"choices":[{"delta":{"content":"world"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"q\":\"a\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":6}}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var out bytes.Buffer
	usage, err := TranslateStream(strings.NewReader(input), &out, "anthropic/claude-haiku-4.5")
	if err != nil {
		t.Fatalf("TranslateStream() error = %v\n%s", err, out.String())
	}
	want := "" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"anthropic/claude-haiku-4.5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"q\":\"a\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4,"cache_read_input_tokens":6}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	if got := out.String(); got != want {
		t.Fatalf("stream =\n%s\nwant\n%s", got, want)
	}
	if !usage.HasUsage || usage.PromptTokens != 10 || usage.CompletionTokens != 4 || usage.CachedTokens != 6 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestTranslateStreamInterleavedToolCallsKeepBlocksOpen(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"first","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"second","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var out bytes.Buffer
	if _, err := TranslateStream(strings.NewReader(input), &out, "claude"); err != nil {
		t.Fatalf("TranslateStream() error = %v\n%s", err, out.String())
	}
	events := parseSSEEvents(t, out.String())
	assertWellFormedContentBlocks(t, events)
	assertEventIndexes(t, events, "content_block_start", []int{0, 1})
	assertEventIndexes(t, events, "content_block_stop", []int{0, 1})
	if got := partialJSONByIndex(t, events); got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("partial JSON by index = %#v", got)
	}
	if got := stopReason(t, events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}

func TestTranslateStreamContentAfterToolCallKeepsBlocksOpen(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello "}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var out bytes.Buffer
	if _, err := TranslateStream(strings.NewReader(input), &out, "claude"); err != nil {
		t.Fatalf("TranslateStream() error = %v\n%s", err, out.String())
	}
	events := parseSSEEvents(t, out.String())
	assertWellFormedContentBlocks(t, events)
	assertEventIndexes(t, events, "content_block_start", []int{0, 1})
	assertEventIndexes(t, events, "content_block_stop", []int{0, 1})
	if got := textByIndex(t, events)[0]; got != "hello done" {
		t.Fatalf("text index 0 = %q, want %q", got, "hello done")
	}
	if got := partialJSONByIndex(t, events)[1]; got != `{}` {
		t.Fatalf("tool JSON index 1 = %q, want {}", got)
	}
	if got := stopReason(t, events); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}

func TestTranslateStreamToolCallsForceToolUseStopReason(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	var out bytes.Buffer
	if _, err := TranslateStream(strings.NewReader(input), &out, "claude"); err != nil {
		t.Fatalf("TranslateStream() error = %v\n%s", err, out.String())
	}
	if got := stopReason(t, parseSSEEvents(t, out.String())); got != "tool_use" {
		t.Fatalf("stream stop_reason = %q, want tool_use", got)
	}
}

func TestTranslateResponseToolCallsForceStopAndParseMultipleInputs(t *testing.T) {
	withFixedMessageID(t, "msg_test")

	raw := []byte(`{
		"model":"llama3",
		"choices":[{
			"message":{
				"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}],
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"a\"}"}},
					{"id":"call_2","type":"function","function":{"name":"save","arguments":"{\"n\":2}"}}
				]
			},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":11,"completion_tokens":3}
	}`)
	got, _, err := TranslateResponse(raw, "anthropic/claude-haiku-4.5")
	if err != nil {
		t.Fatalf("TranslateResponse() error = %v", err)
	}
	want := []byte(`{
		"id":"msg_test",
		"type":"message",
		"role":"assistant",
		"model":"anthropic/claude-haiku-4.5",
		"content":[
			{"type":"text","text":"hello world"},
			{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"a"}},
			{"type":"tool_use","id":"call_2","name":"save","input":{"n":2}}
		],
		"stop_reason":"tool_use",
		"stop_sequence":null,
		"usage":{"input_tokens":11,"output_tokens":3}
	}`)
	assertJSONEqual(t, got, want)
}

func TestTranslateRequestAssistantToolOnlyContentIsEmptyString(t *testing.T) {
	got, err := TranslateRequest([]byte(`{
		"model":"llama3",
		"max_tokens":8,
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"a"}}]}]
	}`))
	if err != nil {
		t.Fatalf("TranslateRequest() error = %v", err)
	}
	var payload openAIChatRequest
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("translated JSON: %v\n%s", err, got)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages = %#v", payload.Messages)
	}
	content, ok := payload.Messages[0].Content.(string)
	if !ok || content != "" {
		t.Fatalf("assistant content = %#v (%T), want empty string", payload.Messages[0].Content, payload.Messages[0].Content)
	}
}

func TestTranslateStreamReasoningAndMalformedChunk(t *testing.T) {
	withFixedMessageID(t, "msg_test")

	var reasoning bytes.Buffer
	_, err := TranslateStream(strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), &reasoning, "claude")
	if err != nil {
		t.Fatalf("TranslateStream(reasoning) error = %v", err)
	}
	if got := reasoning.String(); !strings.Contains(got, `"content_block":{"type":"thinking","thinking":""}`) || !strings.Contains(got, `"type":"thinking_delta","thinking":"think"`) {
		t.Fatalf("reasoning stream missing thinking events:\n%s", got)
	}

	var malformed bytes.Buffer
	_, err = TranslateStream(strings.NewReader("data: {\"choices\":\n\n"), &malformed, "claude")
	if err == nil {
		t.Fatal("TranslateStream(malformed) error = nil")
	}
	if got := malformed.String(); !strings.Contains(got, "event: error") || !strings.Contains(got, "malformed OpenAI stream chunk") {
		t.Fatalf("malformed stream missing error event:\n%s", got)
	}
}

func withFixedMessageID(t *testing.T, id string) {
	t.Helper()
	previous := makeMessageID
	makeMessageID = func() string { return id }
	t.Cleanup(func() { makeMessageID = previous })
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

type sseEvent struct {
	name string
	data map[string]any
}

func parseSSEEvents(t *testing.T, stream string) []sseEvent {
	t.Helper()
	records := strings.Split(strings.TrimSpace(stream), "\n\n")
	events := make([]sseEvent, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record) == "" {
			continue
		}
		var event sseEvent
		for _, line := range strings.Split(record, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.data); err != nil {
					t.Fatalf("SSE data JSON: %v\nrecord:\n%s", err, record)
				}
			}
		}
		if event.name == "" || event.data == nil {
			t.Fatalf("malformed SSE record:\n%s", record)
		}
		events = append(events, event)
	}
	return events
}

func assertWellFormedContentBlocks(t *testing.T, events []sseEvent) {
	t.Helper()
	started := map[int]bool{}
	stopped := map[int]bool{}
	for _, event := range events {
		switch event.name {
		case "content_block_start":
			index := eventIndex(t, event)
			if started[index] {
				t.Fatalf("content block %d started more than once", index)
			}
			if stopped[index] {
				t.Fatalf("content block %d started after stop", index)
			}
			started[index] = true
		case "content_block_delta":
			index := eventIndex(t, event)
			if !started[index] {
				t.Fatalf("content block %d received delta before start", index)
			}
			if stopped[index] {
				t.Fatalf("content block %d received delta after stop", index)
			}
		case "content_block_stop":
			index := eventIndex(t, event)
			if !started[index] {
				t.Fatalf("content block %d stopped before start", index)
			}
			if stopped[index] {
				t.Fatalf("content block %d stopped more than once", index)
			}
			stopped[index] = true
		}
	}
	for index := range started {
		if !stopped[index] {
			t.Fatalf("content block %d was not stopped", index)
		}
	}
}

func assertEventIndexes(t *testing.T, events []sseEvent, name string, want []int) {
	t.Helper()
	got := []int{}
	for _, event := range events {
		if event.name == name {
			got = append(got, eventIndex(t, event))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s indexes = %#v, want %#v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s indexes = %#v, want %#v", name, got, want)
		}
	}
}

func partialJSONByIndex(t *testing.T, events []sseEvent) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, event := range events {
		if event.name != "content_block_delta" {
			continue
		}
		delta, ok := event.data["delta"].(map[string]any)
		if !ok || delta["type"] != "input_json_delta" {
			continue
		}
		partial, _ := delta["partial_json"].(string)
		out[eventIndex(t, event)] += partial
	}
	return out
}

func textByIndex(t *testing.T, events []sseEvent) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, event := range events {
		if event.name != "content_block_delta" {
			continue
		}
		delta, ok := event.data["delta"].(map[string]any)
		if !ok || delta["type"] != "text_delta" {
			continue
		}
		text, _ := delta["text"].(string)
		out[eventIndex(t, event)] += text
	}
	return out
}

func stopReason(t *testing.T, events []sseEvent) string {
	t.Helper()
	for _, event := range events {
		if event.name != "message_delta" {
			continue
		}
		delta, ok := event.data["delta"].(map[string]any)
		if !ok {
			t.Fatalf("message_delta missing delta: %#v", event.data)
		}
		reason, _ := delta["stop_reason"].(string)
		return reason
	}
	t.Fatal("message_delta not found")
	return ""
}

func eventIndex(t *testing.T, event sseEvent) int {
	t.Helper()
	value, ok := event.data["index"]
	if !ok {
		t.Fatalf("%s missing index: %#v", event.name, event.data)
	}
	switch n := value.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			t.Fatalf("index is not an integer: %v", err)
		}
		return int(i)
	default:
		t.Fatalf("index has type %T: %#v", value, value)
		return 0
	}
}
