package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

func init() {
	RegisterAPIStream(APIAnthropicMessages, anthropicMessagesStream)
}

const anthropicVersion = "2023-06-01"

// anthropicMessagesStream streams a request to the Anthropic Messages API
// (POST /messages), mapping its SSE events onto the unified event protocol.
func anthropicMessagesStream(model *Model, ctx *Context, opts *StreamOptions) *EventStream {
	s := NewEventStream(32)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				endStream(s, makeErrorMessage(model, StopReasonError, fmt.Sprintf("internal error: %v", r)))
			}
		}()
		headers := map[string]string{
			"x-api-key":         opts.APIKey,
			"anthropic-version": anthropicVersion,
		}
		applyImpersonation(headers, model)
		for k, v := range ApplyModelHeaders(model, opts) {
			headers[k] = v
		}
		applySessionAffinityHeaders(headers, model, opts)
		// stream-scoped context: the stall watchdog aborts the request on a
		// silent provider, while the parent (run) context keeps user aborts
		// working.
		streamCtx, streamCancel := context.WithCancel(requestCtx(opts))
		defer streamCancel()
		resp, err := doStreamRequestRetry(streamCtx, model.BaseURL+"/messages", headers,
			func() ([]byte, error) { return buildAnthropicBody(model, ctx, opts) })
		if err != nil {
			endStream(s, requestFailureMessage(model, opts, err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			endStream(s, makeErrorMessage(model, StopReasonError,
				formatHTTPError(resp.StatusCode, []byte(readErrorBody(resp)))))
			return
		}

		h := &anthropicHandler{
			model:        model,
			opts:         opts,
			stream:       s,
			partial:      NewAssistantMessage(model.API, model.Provider, model.ID),
			argsJSON:     map[int]string{},
			initialInput: map[int]map[string]any{},
			accums:       map[int]*textAccum{},
		}
		err = IterateSSE(stallWrapped(resp.Body, streamCancel), h.handle)
		msg := h.partial
		// Terminal-state writes take the message lock: the partial may
		// already be marshaled by concurrent event consumers.
		h.partial.mu.Lock()
		switch {
		case errors.Is(err, errStopSSE):
			// Terminal state recorded.
		case errors.Is(err, ErrStallTimeout):
			msg.StopReason = StopReasonError
			msg.ErrorMessage = stallMessage()
		case requestCtx(opts).Err() != nil:
			msg.StopReason = StopReasonAborted
			msg.ErrorMessage = "aborted"
		case err != nil:
			msg.StopReason = StopReasonError
			msg.ErrorMessage = "stream error: " + err.Error()
		default:
			msg.StopReason = StopReasonError
			msg.ErrorMessage = "stream ended without a terminal event"
		}
		// Parse any pending tool call arguments before finishing.
		h.finalizeToolArgs()
		h.partial.mu.Unlock()
		endStream(s, msg)
	}()
	return s
}

// buildAnthropicBody converts the unified context into a Messages API
// request payload.
func buildAnthropicBody(model *Model, ctx *Context, opts *StreamOptions) ([]byte, error) {
	body := map[string]any{
		"model":      model.ID,
		"max_tokens": computeMaxTokens(model, ctx, opts),
		"messages":   convertAnthropicMessages(ctx.Messages),
		"stream":     true,
	}
	if ctx.SystemPrompt != "" {
		body["system"] = ctx.SystemPrompt
	}
	if len(ctx.Tools) > 0 {
		tools := make([]any, 0, len(ctx.Tools))
		for _, t := range ctx.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		body["tools"] = tools
	}
	if budget := resolveAnthropicThinking(model, opts); budget > 0 {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	return json.Marshal(body)
}

// resolveAnthropicThinking maps the unified thinking level to an Anthropic
// thinking token budget. Returns 0 when reasoning is disabled.
func resolveAnthropicThinking(model *Model, opts *StreamOptions) int {
	if opts == nil || opts.Reasoning == "" || opts.Reasoning == "off" {
		return 0
	}
	if v, ok := model.ThinkingLevelMap[opts.Reasoning]; ok && v != nil {
		if n, ok := v.(float64); ok && n > 0 {
			return int(n)
		}
		if n, ok := v.(int); ok && n > 0 {
			return n
		}
	}
	switch opts.Reasoning {
	case "minimal", "low":
		return 1024
	case "medium":
		return 2048
	default:
		return 4096
	}
}

// convertAnthropicMessages converts unified messages into Messages API
// messages. Tool results become user messages with tool_result blocks.
func convertAnthropicMessages(messages []*Message) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleUser:
			out = append(out, map[string]any{"role": "user", "content": convertAnthropicUserContent(m.Content)})
		case RoleAssistant:
			blocks := []any{}
			for _, b := range m.Content {
				switch b.Type {
				case ContentTypeText:
					blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
				case ContentTypeThinking:
					block := map[string]any{"type": "thinking", "thinking": b.Thinking}
					if b.ThinkingSignature != "" {
						block["signature"] = b.ThinkingSignature
					}
					blocks = append(blocks, block)
				case ContentTypeToolCall:
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    b.ToolCallID,
						"name":  b.ToolName,
						"input": b.Arguments,
					})
				}
			}
			// A pure-abort marker (no text, no tool calls, no thinking) carries
			// no information for the model, and Anthropic rejects empty content
			// arrays — drop it from the request history.
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case RoleToolResult:
			content := convertAnthropicToolResult(m)
			out = append(out, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     content,
				}},
			})
		}
	}
	return out
}

func convertAnthropicUserContent(content []ContentBlock) any {
	if len(content) == 1 && content[0].Type == ContentTypeText {
		return content[0].Text
	}
	blocks := []any{}
	for _, b := range content {
		switch b.Type {
		case ContentTypeText:
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case ContentTypeImage:
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.MimeType,
					"data":       base64.StdEncoding.EncodeToString(b.Data),
				},
			})
		}
	}
	return blocks
}

// convertAnthropicToolResult renders a tool result as either a string or a
// content block array.
func convertAnthropicToolResult(m *Message) any {
	if len(m.Content) == 1 && m.Content[0].Type == ContentTypeText {
		return m.Content[0].Text
	}
	blocks := []any{}
	for _, b := range m.Content {
		switch b.Type {
		case ContentTypeText:
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case ContentTypeImage:
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.MimeType,
					"data":       base64.StdEncoding.EncodeToString(b.Data),
				},
			})
		}
	}
	return blocks
}

// anthropicHandler accumulates the streaming Messages response.
type anthropicHandler struct {
	model *Model
	opts  *StreamOptions

	stream *EventStream

	partial      *Message
	emittedStart bool

	// content index → raw accumulated tool input JSON
	argsJSON map[int]string
	// content index → initial tool input from content_block_start
	initialInput map[int]map[string]any
	// content index of thinking blocks → signature
	thinkingSig map[int]string
	// accums holds the per-content-block text accumulators so streaming text
	// and thinking appends don't re-copy the whole block on every delta.
	accums map[int]*textAccum

	// pending batches the events of a locked mutation phase, flushed after
	// the lock is released (see emit/flush).
	pending []AssistantMessageEvent
}

// pending batches the events produced by a locked mutation phase; they are
// flushed after the lock is released so a full stream buffer can never block
// the producer while it holds partial.mu (see completionsHandler).
func (h *anthropicHandler) emit(evs ...AssistantMessageEvent) {
	h.pending = append(h.pending, evs...)
}

func (h *anthropicHandler) flush() {
	for _, ev := range h.pending {
		h.stream.Push(ev)
	}
	h.pending = h.pending[:0]
}

func (h *anthropicHandler) handle(ev SSEEvent) error {
	if ev.Event == "" {
		return nil
	}
	if ctx := requestCtx(h.opts); ctx.Err() != nil {
		return ctx.Err()
	}
	h.partial.mu.Lock()
	err := h.handleLocked(ev)
	h.partial.mu.Unlock()
	h.flush()
	return err
}

func (h *anthropicHandler) handleLocked(ev SSEEvent) error {
	h.emitStart()

	switch ev.Event {
	case "message_start":
		var r struct {
			Message struct {
				ID    string `json:"id"`
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					CacheCreate  int `json:"cache_creation_input_tokens"`
					CacheRead    int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		h.partial.ResponseID = r.Message.ID
		if u := r.Message.Usage; u != nil {
			// Anthropic reports input_tokens as the non-cached portion;
			// cache_read and cache_creation are disjoint, additive parts of
			// the prompt. Normalize Input to the TOTAL prompt (incl. cache)
			// so it matches the OpenAI-family semantics (prompt_tokens incl.
			// cached) used by the cost and stats layers.
			totalInput := u.InputTokens + u.CacheRead + u.CacheCreate
			h.partial.Usage = &Usage{
				Input:       totalInput,
				Output:      u.OutputTokens,
				CacheWrite:  u.CacheCreate,
				CacheRead:   u.CacheRead,
				TotalTokens: totalInput + u.OutputTokens,
			}
			h.partial.Usage.Cost = CalculateCost(*h.partial.Usage, h.model.Cost)
		}

	case "content_block_start":
		var r struct {
			Index int `json:"index"`
			Block struct {
				Type  string         `json:"type"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		// The index is authoritative; backfill any gaps the server skipped.
		for len(h.partial.Content) <= r.Index {
			h.partial.Content = append(h.partial.Content, ContentBlock{})
		}
		switch r.Block.Type {
		case "text":
			h.partial.Content[r.Index] = ContentBlock{Type: ContentTypeText}
			h.emit(AssistantMessageEvent{
				Type: EventTextStart, Partial: h.partial, ContentIndex: r.Index,
			})
		case "thinking":
			h.partial.Content[r.Index] = ContentBlock{Type: ContentTypeThinking}
			h.emit(AssistantMessageEvent{
				Type: EventThinkingStart, Partial: h.partial, ContentIndex: r.Index,
			})
		case "tool_use":
			// Note: construct the block directly so Arguments stays nil until
			// the block is finalized (ToolCallBlock would normalize nil to {}).
			h.partial.Content[r.Index] = ContentBlock{
				Type: ContentTypeToolCall, ToolCallID: r.Block.ID, ToolName: r.Block.Name,
			}
			if len(r.Block.Input) > 0 {
				h.initialInput[r.Index] = r.Block.Input
				h.argsJSON[r.Index] = marshalJSONString(r.Block.Input)
			} else {
				h.initialInput[r.Index] = nil
				h.argsJSON[r.Index] = ""
			}
			h.emit(AssistantMessageEvent{
				Type: EventToolCallStart, Partial: h.partial, ContentIndex: r.Index,
			})
		}

	case "content_block_delta":
		var r struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		if r.Index < 0 || r.Index >= len(h.partial.Content) {
			return nil
		}
		block := &h.partial.Content[r.Index]
		switch r.Delta.Type {
		case "text_delta":
			block.Text = blockAccum(h.accums, h.partial.Content, r.Index).Append(r.Delta.Text)
			h.emit(AssistantMessageEvent{
				Type: EventTextDelta, Partial: h.partial, ContentIndex: r.Index, Delta: r.Delta.Text,
			})
		case "thinking_delta":
			block.Thinking = blockAccum(h.accums, h.partial.Content, r.Index).Append(r.Delta.Thinking)
			h.emit(AssistantMessageEvent{
				Type: EventThinkingDelta, Partial: h.partial, ContentIndex: r.Index, Delta: r.Delta.Thinking,
			})
		case "input_json_delta":
			if block.Type == ContentTypeToolCall {
				h.argsJSON[r.Index] += r.Delta.PartialJSON
				h.emit(AssistantMessageEvent{
					Type: EventToolCallDelta, Partial: h.partial, ContentIndex: r.Index, Delta: r.Delta.PartialJSON,
				})
			}
		}

	case "content_block_stop":
		var r struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		if r.Index < 0 || r.Index >= len(h.partial.Content) {
			return nil
		}
		block := h.partial.Content[r.Index]
		switch block.Type {
		case ContentTypeText:
			h.emit(AssistantMessageEvent{
				Type: EventTextEnd, Partial: h.partial, ContentIndex: r.Index,
			})
		case ContentTypeThinking:
			h.emit(AssistantMessageEvent{
				Type: EventThinkingEnd, Partial: h.partial, ContentIndex: r.Index,
			})
		case ContentTypeToolCall:
			args, err := parseArguments(h.argsJSON[r.Index])
			if err != nil {
				if init := h.initialInput[r.Index]; init != nil {
					args = init
				} else {
					args = map[string]any{}
				}
			}
			h.partial.Content[r.Index].Arguments = args
			h.emit(AssistantMessageEvent{
				Type: EventToolCallEnd, Partial: h.partial, ContentIndex: r.Index,
				ToolCall: toolCallBlockCopy(h.partial.Content[r.Index]),
			})
		}

	case "message_delta":
		var r struct {
			Delta struct {
				StopReason        string `json:"stop_reason"`
				ThinkingSignature string `json:"thinking_signature"`
			} `json:"delta"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		if r.Delta.ThinkingSignature != "" {
			// Apply the signature to the last thinking block.
			for i := len(h.partial.Content) - 1; i >= 0; i-- {
				if h.partial.Content[i].Type == ContentTypeThinking {
					h.partial.Content[i].ThinkingSignature = r.Delta.ThinkingSignature
					break
				}
			}
		}
		if h.partial.Usage == nil {
			h.partial.Usage = &Usage{}
		}
		if r.Usage != nil {
			h.partial.Usage.Output = r.Usage.OutputTokens
			h.partial.Usage.TotalTokens = h.partial.Usage.Input + h.partial.Usage.Output
			h.partial.Usage.Cost = CalculateCost(*h.partial.Usage, h.model.Cost)
		}
		h.partial.StopReason = mapAnthropicStopReason(r.Delta.StopReason)

	case "message_stop":
		h.finalizeToolArgs()
		return errStopSSE

	case "error":
		var r struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &r)
		h.partial.StopReason = StopReasonError
		h.partial.ErrorMessage = r.Error.Message
		if h.partial.ErrorMessage == "" {
			h.partial.ErrorMessage = "provider returned an error event"
		}
		return errStopSSE
	}
	return nil
}

// finalizeToolArgs parses any tool call arguments that were not finalized by
// content_block_stop (e.g. a stream that ended without message_stop).
func (h *anthropicHandler) finalizeToolArgs() {
	for i := range h.partial.Content {
		if h.partial.Content[i].Type != ContentTypeToolCall {
			continue
		}
		if h.partial.Content[i].Arguments != nil {
			continue
		}
		args, err := parseArguments(h.argsJSON[i])
		if err != nil {
			if init := h.initialInput[i]; init != nil {
				args = init
			} else {
				args = map[string]any{}
			}
		}
		h.partial.Content[i].Arguments = args
	}
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return StopReasonStop
	case "max_tokens":
		return StopReasonLength
	case "tool_use":
		return StopReasonToolUse
	case "refusal":
		return StopReasonStop
	case "":
		return ""
	default:
		return StopReasonStop
	}
}

func (h *anthropicHandler) emitStart() {
	if h.emittedStart {
		return
	}
	h.emittedStart = true
	h.emit(AssistantMessageEvent{Type: EventStart, Partial: h.partial})
}
