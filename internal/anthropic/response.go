package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

type openAIChatResponse struct {
	Model   string           `json:"model"`
	Choices []openAIChoice   `json:"choices"`
	Usage   *openAIUsageBody `json:"usage"`
}

type openAIChoice struct {
	Message      openAIChoiceMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

type openAIChoiceMessage struct {
	Content          json.RawMessage          `json:"content"`
	ReasoningContent string                   `json:"reasoning_content"`
	Reasoning        string                   `json:"reasoning"`
	ToolCalls        []openAIResponseToolCall `json:"tool_calls"`
}

// reasoningText returns the chain-of-thought a reasoning model attached to a
// non-streaming response, preferring the reasoning_content spelling used by
// DeepSeek/Kimi over the plain reasoning field.
func (m openAIChoiceMessage) reasoningText() string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

type openAIResponseToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsageBody struct {
	PromptTokens            int64                   `json:"prompt_tokens"`
	CompletionTokens        int64                   `json:"completion_tokens"`
	TotalTokens             int64                   `json:"total_tokens"`
	PromptTokensDetails     *promptTokenDetails     `json:"prompt_tokens_details"`
	CachedTokensTop         int64                   `json:"cached_tokens"`
	PromptCacheHitTokens    int64                   `json:"prompt_cache_hit_tokens"`
	CompletionTokensDetails *completionTokenDetails `json:"completion_tokens_details"`
}

type promptTokenDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type completionTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

func (u *openAIUsageBody) cachedTokens() int64 {
	if u == nil {
		return 0
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	if u.CachedTokensTop > 0 {
		return u.CachedTokensTop
	}
	return u.PromptCacheHitTokens
}

type messageResponse struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []responseBlock `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        responseUsage   `json:"usage"`
}

type responseUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens,omitempty"`
}

type responseBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Input    any    `json:"input,omitempty"`
}

// TranslateResponse converts a non-streaming OpenAI chat completion response
// from the local leg into an Anthropic MessageResponse.
func TranslateResponse(raw []byte, visibleModel string) ([]byte, Usage, error) {
	var in openAIChatResponse
	if err := decodeUseNumber(raw, &in); err != nil {
		return nil, Usage{}, apiError(502, fmt.Sprintf("decode local response: %v", err))
	}
	var choice openAIChoice
	if len(in.Choices) > 0 {
		choice = in.Choices[0]
	}

	blocks := make([]responseBlock, 0, 2+len(choice.Message.ToolCalls))
	if reasoning := choice.Message.reasoningText(); reasoning != "" {
		blocks = append(blocks, responseBlock{Type: "thinking", Thinking: reasoning})
	}
	if text := responseContentText(choice.Message.Content); text != "" {
		blocks = append(blocks, responseBlock{Type: "text", Text: text})
	}
	for _, call := range choice.Message.ToolCalls {
		input := parseToolInput(call.Function.Arguments)
		blocks = append(blocks, responseBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	if blocks == nil {
		blocks = []responseBlock{}
	}

	usage := Usage{}
	if in.Usage != nil {
		usage = Usage{
			PromptTokens:     in.Usage.PromptTokens,
			CompletionTokens: in.Usage.CompletionTokens,
			CachedTokens:     in.Usage.cachedTokens(),
			HasUsage:         true,
		}
	}
	stopReason := mapOpenAIFinishReason(choice.FinishReason)
	if len(choice.Message.ToolCalls) > 0 {
		stopReason = "tool_use"
	}
	out := messageResponse{
		ID:         makeMessageID(),
		Type:       "message",
		Role:       "assistant",
		Model:      visibleModel,
		Content:    blocks,
		StopReason: stopReason,
		Usage: responseUsage{
			InputTokens:          usage.PromptTokens,
			OutputTokens:         usage.CompletionTokens,
			CacheReadInputTokens: usage.CachedTokens,
		},
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, Usage{}, err
	}
	return body, usage, nil
}

func responseContentText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out strings.Builder
		for _, part := range parts {
			if part.Text != "" {
				out.WriteString(part.Text)
			}
		}
		return out.String()
	}
	return ""
}

func parseToolInput(arguments string) any {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}
	}
	var input any
	if err := decodeUseNumber([]byte(arguments), &input); err != nil {
		log.Printf("bursty anthropic: local tool_call arguments were not valid JSON: %v", err)
		return map[string]any{}
	}
	return input
}

func mapOpenAIFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		// "refusal" is the Anthropic-valid stop_reason for a filtered/blocked
		// turn (matches the enclave); mapping to end_turn would hide the stop.
		return "refusal"
	default:
		return "end_turn"
	}
}
