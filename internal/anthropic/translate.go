package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const imageUnsupportedMessage = "images not supported on the local path; remove or route to cloud with provider.order"

// Error is an Anthropic-shaped translation error suitable for the /v1/messages
// endpoint.
type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// Usage is the token accounting extracted while translating an OpenAI-shaped
// local response back to Anthropic shape.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	HasUsage         bool
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	MaxTokens     *int               `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	StopSequences []string           `json:"stop_sequences"`
	Thinking      json.RawMessage    `json:"thinking"`
	Stream        bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type contentBlock struct {
	Type      string
	Text      string
	ID        string
	Name      string
	Input     any
	ToolUseID string
	Content   any
	IsError   bool
}

type openAIChatRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	Tools           []openAITool    `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	MaxTokens       int             `json:"max_tokens"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	TopK            *int            `json:"top_k,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Stream          bool            `json:"stream"`
	StreamOptions   *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

var anthropicLogOnce sync.Map
var makeMessageID = newMessageID

// TranslateRequest converts an Anthropic /v1/messages request into an
// OpenAI-compatible /v1/chat/completions request for the local leg.
func TranslateRequest(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := decodeUseNumber(raw, &fields); err != nil {
		return nil, invalidRequest(fmt.Sprintf("decode request body: %v", err))
	}
	logDroppedTopLevelFields(fields)

	var req anthropicRequest
	if err := decodeUseNumber(raw, &req); err != nil {
		return nil, invalidRequest(fmt.Sprintf("decode request body: %v", err))
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, invalidRequest("model is required")
	}
	if req.MaxTokens == nil {
		return nil, invalidRequest("max_tokens is required")
	}

	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	if len(bytes.TrimSpace(req.System)) > 0 && !bytes.Equal(bytes.TrimSpace(req.System), []byte("null")) {
		text, err := systemText(req.System)
		if err != nil {
			return nil, err
		}
		if text != "" {
			messages = append(messages, openAIMessage{Role: "system", Content: text})
		}
	}
	for _, message := range req.Messages {
		translated, err := translateMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, translated...)
	}

	out := openAIChatRequest{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   *req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		TopK:        req.TopK,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
	}
	if effort := reasoningEffortFromThinking(req.Thinking); effort != "" {
		out.ReasoningEffort = effort
	}
	if req.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]openAITool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			out.Tools = append(out.Tools, openAITool{
				Type: "function",
				Function: openAIToolFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.InputSchema,
				},
			})
		}
	}
	if len(bytes.TrimSpace(req.ToolChoice)) > 0 {
		choice, err := translateToolChoice(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = choice
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func translateMessage(message anthropicMessage) ([]openAIMessage, error) {
	role := strings.TrimSpace(message.Role)
	if role != "user" && role != "assistant" {
		return nil, invalidRequest(fmt.Sprintf("unsupported message role %q", role))
	}
	if len(bytes.TrimSpace(message.Content)) == 0 || bytes.Equal(bytes.TrimSpace(message.Content), []byte("null")) {
		return []openAIMessage{{Role: role, Content: ""}}, nil
	}
	var contentString string
	if err := json.Unmarshal(message.Content, &contentString); err == nil {
		return []openAIMessage{{Role: role, Content: contentString}}, nil
	}
	blocks, err := parseContentBlocks(message.Content)
	if err != nil {
		return nil, err
	}

	hasToolUse := false
	hasToolResult := false
	for _, block := range blocks {
		if block.Type == "tool_use" {
			hasToolUse = true
		}
		if block.Type == "tool_result" {
			hasToolResult = true
		}
	}
	switch {
	case role == "assistant" && hasToolUse:
		var text strings.Builder
		toolCalls := make([]openAIToolCall, 0, len(blocks))
		for _, block := range blocks {
			switch block.Type {
			case "text":
				text.WriteString(block.Text)
			case "tool_use":
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   block.ID,
					Type: "function",
					Function: openAIFunction{
						Name:      block.Name,
						Arguments: toolUseArguments(block.Input),
					},
				})
			}
		}
		return []openAIMessage{{
			Role:      "assistant",
			Content:   text.String(),
			ToolCalls: toolCalls,
		}}, nil
	case role == "user" && hasToolResult:
		out := make([]openAIMessage, 0, len(blocks))
		var leftover strings.Builder
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				content, err := toolResultText(block.Content)
				if err != nil {
					return nil, err
				}
				if block.IsError {
					content = "[error] " + content
				}
				out = append(out, openAIMessage{
					Role:       "tool",
					ToolCallID: block.ToolUseID,
					Content:    content,
				})
			case "text":
				leftover.WriteString(block.Text)
			}
		}
		if strings.TrimSpace(leftover.String()) != "" {
			out = append(out, openAIMessage{Role: "user", Content: leftover.String()})
		}
		return out, nil
	default:
		var text strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		return []openAIMessage{{Role: role, Content: text.String()}}, nil
	}
}

func systemText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	blocks, err := parseContentBlocks(raw)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	return out.String(), nil
}

func parseContentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	var rawBlocks []json.RawMessage
	if err := decodeUseNumber(raw, &rawBlocks); err != nil {
		return nil, invalidRequest(fmt.Sprintf("content must be a string or content block array: %v", err))
	}
	blocks := make([]contentBlock, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		block, err := parseContentBlock(rawBlock)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func parseContentBlock(raw json.RawMessage) (contentBlock, error) {
	var fields map[string]json.RawMessage
	if err := decodeUseNumber(raw, &fields); err != nil {
		return contentBlock{}, invalidRequest(fmt.Sprintf("content block must be an object: %v", err))
	}
	if _, ok := fields["cache_control"]; ok {
		logOnce("cache_control", "bursty anthropic: dropping cache_control hints on local /v1/messages path")
	}
	blockType := rawString(fields["type"])
	switch blockType {
	case "image":
		return contentBlock{}, invalidRequest(imageUnsupportedMessage)
	case "text":
		return contentBlock{Type: "text", Text: rawString(fields["text"])}, nil
	case "tool_use":
		var input any
		if rawInput, ok := fields["input"]; ok {
			if err := decodeUseNumber(rawInput, &input); err != nil {
				return contentBlock{}, invalidRequest(fmt.Sprintf("tool_use input must be JSON: %v", err))
			}
		}
		return contentBlock{
			Type:  "tool_use",
			ID:    rawString(fields["id"]),
			Name:  rawString(fields["name"]),
			Input: input,
		}, nil
	case "tool_result":
		var content any
		if rawContent, ok := fields["content"]; ok {
			if err := decodeUseNumber(rawContent, &content); err != nil {
				return contentBlock{}, invalidRequest(fmt.Sprintf("tool_result content must be JSON: %v", err))
			}
		}
		isError := false
		if rawIsError, ok := fields["is_error"]; ok {
			_ = json.Unmarshal(rawIsError, &isError)
		}
		return contentBlock{
			Type:      "tool_result",
			ToolUseID: rawString(fields["tool_use_id"]),
			Content:   content,
			IsError:   isError,
		}, nil
	default:
		logOnce("content_block:"+blockType, fmt.Sprintf("bursty anthropic: dropping unsupported content block type %q on local /v1/messages path", blockType))
		return contentBlock{Type: blockType}, nil
	}
}

func toolUseArguments(input any) string {
	if input == nil {
		return "{}"
	}
	if s, ok := input.(string); ok {
		return s
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func toolResultText(content any) (string, error) {
	switch value := content.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case []any:
		var out strings.Builder
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if blockType, _ := block["type"].(string); blockType == "image" {
				return "", invalidRequest(imageUnsupportedMessage)
			}
			if _, ok := block["cache_control"]; ok {
				logOnce("cache_control", "bursty anthropic: dropping cache_control hints on local /v1/messages path")
			}
			if text, _ := block["text"].(string); text != "" {
				out.WriteString(text)
			}
		}
		return out.String(), nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", nil
		}
		return string(encoded), nil
	}
}

// reasoningEffortFromThinking maps an Anthropic extended-thinking directive to
// an OpenAI reasoning_effort string so local servers that honor it (vLLM/SGLang
// reasoning models) receive the hint. Budget thresholds mirror the enclave's
// effort buckets (low<=1024, medium<=4096, high above). Disabled or absent
// thinking yields no hint; servers that ignore reasoning_effort are unaffected.
func reasoningEffortFromThinking(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var cfg struct {
		Type         string       `json:"type"`
		BudgetTokens *json.Number `json:"budget_tokens"`
	}
	if err := decodeUseNumber(raw, &cfg); err != nil {
		return ""
	}
	if cfg.Type != "" && !strings.EqualFold(cfg.Type, "enabled") {
		return ""
	}
	budget := 0
	if cfg.BudgetTokens != nil {
		if n, err := cfg.BudgetTokens.Int64(); err == nil {
			budget = int(n)
		}
	}
	switch {
	case budget <= 0:
		if strings.EqualFold(cfg.Type, "enabled") {
			return "medium"
		}
		return ""
	case budget <= 1024:
		return "low"
	case budget <= 4096:
		return "medium"
	default:
		return "high"
	}
}

func translateToolChoice(raw json.RawMessage) (any, error) {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := decodeUseNumber(raw, &choice); err != nil {
		return nil, invalidRequest(fmt.Sprintf("tool_choice must be an object: %v", err))
	}
	switch choice.Type {
	case "", "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "tool":
		if choice.Name == "" {
			return nil, invalidRequest("tool_choice tool name is required")
		}
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": choice.Name,
			},
		}, nil
	default:
		return nil, invalidRequest(fmt.Sprintf("unsupported tool_choice type %q", choice.Type))
	}
}

func logDroppedTopLevelFields(fields map[string]json.RawMessage) {
	known := map[string]struct{}{
		"model":             {},
		"system":            {},
		"messages":          {},
		"tools":             {},
		"tool_choice":       {},
		"max_tokens":        {},
		"temperature":       {},
		"top_p":             {},
		"top_k":             {},
		"stop_sequences":    {},
		"stream":            {},
		"stream_options":    {},
		"provider":          {},
		"anthropic_version": {},
		"metadata":          {},
		"thinking":          {},
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := known[key]; ok {
			switch key {
			case "anthropic_version", "metadata":
				logOnce("top:"+key, fmt.Sprintf("bursty anthropic: dropping %s on local /v1/messages path", key))
			}
			continue
		}
		logOnce("top:"+key, fmt.Sprintf("bursty anthropic: dropping unknown top-level field %q on local /v1/messages path", key))
	}
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var out string
	_ = json.Unmarshal(raw, &out)
	return out
}

func decodeUseNumber(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(v)
}

func invalidRequest(message string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: message}
}

func apiError(status int, message string) *Error {
	return &Error{Status: status, Type: "api_error", Message: message}
}

func logOnce(key, message string) {
	if _, loaded := anthropicLogOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Print(message)
}

func newMessageID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "msg_" + hex.EncodeToString(buf[:])
	}
	return "msg_fallback"
}
