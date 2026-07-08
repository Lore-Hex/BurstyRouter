package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxStreamLineBytes = 1 << 20

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          json.RawMessage             `json:"content"`
			ReasoningContent string                      `json:"reasoning_content"`
			ToolCalls        []openAIStreamToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsageBody `json:"usage"`
}

type openAIStreamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type streamTranslator struct {
	w              io.Writer
	nextBlockIndex int
	textIndex      *int
	thinkingIndex  *int
	toolIndexes    map[int]int
	toolCalls      map[int]*toolAccumulator
	openedBlocks   []int
	openedBlockSet map[int]struct{}
}

type toolAccumulator struct {
	ID        string
	Name      string
	Arguments string
}

// TranslateStream converts OpenAI chat-completions SSE from the local leg into
// Anthropic /v1/messages SSE.
func TranslateStream(r io.Reader, w io.Writer, visibleModel string) (Usage, error) {
	if err := writeEvent(w, "message_start", messageStartEvent{
		Type: "message_start",
		Message: streamMessage{
			ID:      makeMessageID(),
			Type:    "message",
			Role:    "assistant",
			Model:   visibleModel,
			Content: []any{},
			Usage:   streamStartUsage{InputTokens: 0},
		},
	}); err != nil {
		return Usage{}, err
	}

	state := &streamTranslator{
		w:              w,
		toolIndexes:    map[int]int{},
		toolCalls:      map[int]*toolAccumulator{},
		openedBlockSet: map[int]struct{}{},
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLineBytes)

	stopReason := "end_turn"
	var finalUsage Usage
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := decodeUseNumber([]byte(payload), &chunk); err != nil {
			_ = state.closeAllBlocks()
			_ = writeStreamError(w, fmt.Sprintf("malformed OpenAI stream chunk: %v", err))
			return finalUsage, err
		}
		// Latch usage only from a chunk that carries real counts. Some providers
		// send a spec-compliant trailing chunk with usage:{0,0}, which would
		// otherwise clobber the true totals from the last content chunk.
		if chunk.Usage != nil && (chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0) {
			finalUsage = Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				CachedTokens:     chunk.Usage.cachedTokens(),
				HasUsage:         true,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.ReasoningContent != "" {
			if err := state.writeThinkingDelta(choice.Delta.ReasoningContent); err != nil {
				return finalUsage, err
			}
		}
		if text := responseContentText(choice.Delta.Content); text != "" {
			if err := state.writeTextDelta(text); err != nil {
				return finalUsage, err
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			if err := state.writeToolDelta(delta); err != nil {
				return finalUsage, err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			stopReason = mapOpenAIFinishReason(*choice.FinishReason)
		}
	}
	if err := scanner.Err(); err != nil {
		_ = state.closeAllBlocks()
		_ = writeStreamError(w, fmt.Sprintf("read OpenAI stream: %v", err))
		return finalUsage, err
	}
	if err := state.closeAllBlocks(); err != nil {
		return finalUsage, err
	}
	if state.hasToolBlocks() {
		stopReason = "tool_use"
	}
	if err := writeEvent(w, "message_delta", messageDeltaEvent{
		Type:  "message_delta",
		Delta: messageDelta{StopReason: stopReason},
		Usage: streamDeltaUsage{
			OutputTokens:         finalUsage.CompletionTokens,
			CacheReadInputTokens: finalUsage.CachedTokens,
		},
	}); err != nil {
		return finalUsage, err
	}
	if err := writeEvent(w, "message_stop", messageStopEvent{Type: "message_stop"}); err != nil {
		return finalUsage, err
	}
	return finalUsage, nil
}

func (s *streamTranslator) writeTextDelta(text string) error {
	if s.textIndex == nil {
		index := s.nextBlockIndex
		s.nextBlockIndex++
		s.textIndex = &index
		if err := s.startBlock(index, textBlock{Type: "text", Text: ""}); err != nil {
			return err
		}
	}
	return writeEvent(s.w, "content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: *s.textIndex,
		Delta: textDelta{Type: "text_delta", Text: text},
	})
}

func (s *streamTranslator) writeThinkingDelta(text string) error {
	if s.thinkingIndex == nil {
		index := s.nextBlockIndex
		s.nextBlockIndex++
		s.thinkingIndex = &index
		if err := s.startBlock(index, thinkingBlock{Type: "thinking", Thinking: ""}); err != nil {
			return err
		}
	}
	return writeEvent(s.w, "content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: *s.thinkingIndex,
		Delta: thinkingDelta{Type: "thinking_delta", Thinking: text},
	})
}

func (s *streamTranslator) writeToolDelta(delta openAIStreamToolCallDelta) error {
	call := s.toolCalls[delta.Index]
	if call == nil {
		id := delta.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", delta.Index)
		}
		call = &toolAccumulator{ID: id, Name: delta.Function.Name}
		s.toolCalls[delta.Index] = call
		index := s.nextBlockIndex
		s.nextBlockIndex++
		s.toolIndexes[delta.Index] = index
		if err := s.startBlock(index, toolUseBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Name,
			Input: map[string]any{},
		}); err != nil {
			return err
		}
	}
	if call.Name == "" && delta.Function.Name != "" {
		call.Name = delta.Function.Name
	}
	index := s.toolIndexes[delta.Index]
	if delta.Function.Arguments == "" {
		return nil
	}
	call.Arguments += delta.Function.Arguments
	return writeEvent(s.w, "content_block_delta", contentBlockDeltaEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: inputJSONDelta{Type: "input_json_delta", PartialJSON: delta.Function.Arguments},
	})
}

func (s *streamTranslator) startBlock(index int, block any) error {
	if _, ok := s.openedBlockSet[index]; ok {
		return nil
	}
	if err := writeEvent(s.w, "content_block_start", contentBlockStartEvent{
		Type:         "content_block_start",
		Index:        index,
		ContentBlock: block,
	}); err != nil {
		return err
	}
	s.openedBlockSet[index] = struct{}{}
	s.openedBlocks = append(s.openedBlocks, index)
	return nil
}

func (s *streamTranslator) closeAllBlocks() error {
	for _, index := range s.openedBlocks {
		if err := writeEvent(s.w, "content_block_stop", contentBlockStopEvent{
			Type:  "content_block_stop",
			Index: index,
		}); err != nil {
			return err
		}
		delete(s.openedBlockSet, index)
	}
	s.openedBlocks = nil
	return nil
}

func (s *streamTranslator) hasToolBlocks() bool {
	return len(s.toolCalls) > 0
}

func writeStreamError(w io.Writer, message string) error {
	return writeEvent(w, "error", errorEvent{
		Type:  "error",
		Error: streamError{Type: "api_error", Message: message},
	})
}

func writeEvent(w io.Writer, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	return err
}

type messageStartEvent struct {
	Type    string        `json:"type"`
	Message streamMessage `json:"message"`
}

type streamMessage struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []any            `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        streamStartUsage `json:"usage"`
}

type streamStartUsage struct {
	InputTokens int64 `json:"input_tokens"`
}

type contentBlockStartEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type thinkingBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type toolUseBlock struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input any    `json:"input"`
}

type contentBlockDeltaEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type thinkingDelta struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

type inputJSONDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type contentBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaEvent struct {
	Type  string           `json:"type"`
	Delta messageDelta     `json:"delta"`
	Usage streamDeltaUsage `json:"usage"`
}

type messageDelta struct {
	StopReason string `json:"stop_reason"`
}

type streamDeltaUsage struct {
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens,omitempty"`
}

type messageStopEvent struct {
	Type string `json:"type"`
}

type errorEvent struct {
	Type  string      `json:"type"`
	Error streamError `json:"error"`
}

type streamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
