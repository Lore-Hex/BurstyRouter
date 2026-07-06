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
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"q\":\"a\"}"}}` + "\n\n" +
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
