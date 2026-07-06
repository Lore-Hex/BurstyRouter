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
	Content   json.RawMessage          `json:"content"`
	ToolCalls []openAIResponseToolCall `json:"tool_calls"`
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
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type responseBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
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

	blocks := make([]responseBlock, 0, 1+len(choice.Message.ToolCalls))
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
	out := messageResponse{
		ID:         makeMessageID(),
		Type:       "message",
		Role:       "assistant",
		Model:      visibleModel,
		Content:    blocks,
		StopReason: mapOpenAIFinishReason(choice.FinishReason),
		Usage: responseUsage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
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
		logOnce("finish_reason:content_filter", "bursty anthropic: mapping OpenAI content_filter finish_reason to Anthropic end_turn")
		return "end_turn"
	default:
		return "end_turn"
	}
}
