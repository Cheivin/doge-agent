package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func init() {
	RegisterAPIStream(APIOpenAICompletions, openAICompletionsStream)
}

// openAICompletionsStream streams a request to the OpenAI Chat Completions
// API (POST /chat/completions), which is the de-facto standard protocol of
// OpenAI-compatible services (DeepSeek, Qwen, Moonshot, etc.).
func openAICompletionsStream(model *Model, ctx *Context, opts *StreamOptions) *EventStream {
	s := NewEventStream(32)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				endStream(s, makeErrorMessage(model, StopReasonError, fmt.Sprintf("internal error: %v", r)))
			}
		}()
		headers := map[string]string{"Authorization": "Bearer " + opts.APIKey}
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
		resp, err := doStreamRequestRetry(streamCtx, model.BaseURL+"/chat/completions", headers,
			func() ([]byte, error) { return buildCompletionsBody(model, ctx, opts) })
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

		h := &completionsHandler{
			model:        model,
			opts:         opts,
			stream:       s,
			partial:      NewAssistantMessage(model.API, model.Provider, model.ID),
			toolIdx:      map[int]int{}, // streaming tool_calls index → content index
			argsJSON:     map[int]string{},
			reasoningIdx: -1, // no thinking block yet
			accums:       map[int]*textAccum{},
		}
		err = IterateSSE(stallWrapped(resp.Body, streamCancel), h.handle)
		msg := h.partial
		// The partial has already been published to event consumers (they
		// may be marshaling it right now), so terminal-state writes take the
		// message lock like every other mutation.
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
			// EOF without [DONE]: some providers omit the sentinel; treat a
			// clean EOF as a normal stop.
			if msg.StopReason == "" {
				msg.StopReason = StopReasonStop
			}
		}
		h.closeThinking()
		h.finalizeToolCalls()
		h.partial.mu.Unlock()
		h.flush()
		endStream(s, msg)
	}()
	return s
}

// buildCompletionsBody converts the unified context into a chat completions
// request payload.
func buildCompletionsBody(model *Model, ctx *Context, opts *StreamOptions) ([]byte, error) {
	body := map[string]any{
		"model":          model.ID,
		"messages":       convertCompletionsMessages(model, ctx),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"tool_choice":    "auto",
	}
	if len(ctx.Tools) > 0 {
		tools := make([]any, 0, len(ctx.Tools))
		for _, t := range ctx.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		body["tools"] = tools
	}
	if max := computeMaxTokens(model, ctx, opts); max > 0 {
		body["max_tokens"] = max
	}
	return json.Marshal(body)
}

// convertCompletionsMessages converts unified messages into chat completion
// messages. Assistant tool calls become tool_calls; tool results become
// role "tool" messages. For reasoning models, assistant messages carry the
// previous turn's reasoning_content back (DeepSeek-family gateways reject
// thinking-mode requests that omit it).
func convertCompletionsMessages(model *Model, ctx *Context) []any {
	out := make([]any, 0, len(ctx.Messages)+1)
	if ctx.SystemPrompt != "" {
		out = append(out, map[string]any{"role": "system", "content": ctx.SystemPrompt})
	}
	for _, m := range ctx.Messages {
		switch m.Role {
		case RoleUser:
			out = append(out, map[string]any{"role": "user", "content": convertUserContent(m.Content)})
		case RoleAssistant:
			text := assistantText(m)
			reasoning := assistantReasoning(m)
			toolCalls := []any{}
			for _, b := range m.Content {
				if b.Type == ContentTypeToolCall {
					toolCalls = append(toolCalls, map[string]any{
						"id":       b.ToolCallID,
						"type":     "function",
						"function": map[string]any{"name": b.ToolName, "arguments": marshalJSONString(b.Arguments)},
					})
				}
			}
			// A pure-abort marker (no text, no tool calls, no reasoning) carries
			// no information for the model, and gateways reject assistant
			// messages with null content and no tool_calls ("Format Error" 400)
			// — drop it from the request history.
			if text == "" && reasoning == "" && len(toolCalls) == 0 {
				continue
			}
			// content must be null when the assistant produced no text:
			// some gateways (Console Go) reject an empty string here.
			item := map[string]any{"role": "assistant"}
			if text != "" {
				item["content"] = text
			} else {
				item["content"] = nil
			}
			if model.Reasoning {
				item["reasoning_content"] = reasoning
			}
			if len(toolCalls) > 0 {
				item["tool_calls"] = toolCalls
			}
			out = append(out, item)
		case RoleToolResult:
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      toolResultOutput(m),
			})
		}
	}
	return out
}

func assistantText(m *Message) string {
	var text strings.Builder
	for _, b := range m.Content {
		if b.Type == ContentTypeText {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

// assistantReasoning concatenates a message's thinking blocks for the
// reasoning_content round-trip.
func assistantReasoning(m *Message) string {
	var out strings.Builder
	for _, b := range m.Content {
		if b.Type == ContentTypeThinking {
			out.WriteString(b.Thinking)
		}
	}
	return out.String()
}

// completionsHandler accumulates the streaming chat completion.
type completionsHandler struct {
	model *Model
	opts  *StreamOptions

	stream *EventStream

	partial      *Message
	emittedStart bool

	// streaming tool_calls index → content index in partial.Content
	toolIdx map[int]int
	// content index → accumulated raw arguments JSON
	argsJSON map[int]string
	// reasoningIdx is the content index of the in-progress thinking block;
	// -1 once reasoning closed (or never started).
	reasoningIdx int
	// accums holds the per-content-block text accumulators so streaming text
	// and thinking appends don't re-copy the whole block on every delta.
	accums map[int]*textAccum

	// pending batches the events produced by a locked mutation phase. They
	// are flushed to the stream AFTER the lock is released: a full stream
	// buffer must never block the producer while it holds partial.mu, or a
	// consumer waiting on that lock (MarshalJSON) stops draining and the two
	// deadlock.
	pending []AssistantMessageEvent
}

// emit queues an event for the current mutation phase.
func (h *completionsHandler) emit(evs ...AssistantMessageEvent) {
	h.pending = append(h.pending, evs...)
}

// flush pushes the queued events to the stream, draining the batch. Called
// with no lock held.
func (h *completionsHandler) flush() {
	for _, ev := range h.pending {
		h.stream.Push(ev)
	}
	h.pending = h.pending[:0]
}

func (h *completionsHandler) handle(ev SSEEvent) error {
	if ctx := requestCtx(h.opts); ctx.Err() != nil {
		return ctx.Err()
	}
	// Mutations are serialized against readers (MarshalJSON takes the read
	// lock); event emission is deferred to flush() so no channel send ever
	// happens under the lock.
	h.partial.mu.Lock()
	err := h.handleLocked(ev)
	h.partial.mu.Unlock()
	h.flush()
	return err
}

func (h *completionsHandler) handleLocked(ev SSEEvent) error {
	h.emitStart()

	if string(ev.Data) == "[DONE]" {
		if h.partial.StopReason == "" {
			h.partial.StopReason = StopReasonStop
		}
		return errStopSSE
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				Reasoning        *string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function *struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptDetails    *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(ev.Data, &chunk); err != nil {
		return err
	}

	for _, choice := range chunk.Choices {
		if choice.Delta.ReasoningContent != nil {
			h.appendThinking(*choice.Delta.ReasoningContent)
		}
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			// Empty content deltas (some gateways send content:"" on every
			// chunk) must not close the thinking block.
			h.closeThinking()
			h.appendText(*choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			h.closeThinking()
			h.applyToolCallDelta(tc)
		}
		if choice.FinishReason != nil {
			switch *choice.FinishReason {
			case "stop":
				h.partial.StopReason = StopReasonStop
			case "tool_calls":
				h.partial.StopReason = StopReasonToolUse
			case "length":
				h.partial.StopReason = StopReasonLength
			case "content_filter":
				h.partial.StopReason = StopReasonError
				h.partial.ErrorMessage = "response was filtered by the content filter"
			default:
				h.partial.StopReason = StopReasonStop
			}
		}
	}
	if u := chunk.Usage; u != nil {
		usage := &Usage{
			Input:       u.PromptTokens,
			Output:      u.CompletionTokens,
			TotalTokens: u.TotalTokens,
		}
		if u.PromptDetails != nil {
			usage.CacheRead = u.PromptDetails.CachedTokens
		}
		if u.CompletionDetails != nil {
			usage.Reasoning = u.CompletionDetails.ReasoningTokens
		}
		usage.Cost = CalculateCost(*usage, h.model.Cost)
		h.partial.Usage = usage
	}
	// Note: finish_reason does not stop iteration here; the usage chunk may
	// arrive after it (stream_options.include_usage). Iteration ends at
	// [DONE] or EOF.
	return nil
}

// appendThinking accumulates a reasoning_content delta into the thinking
// block, creating it on first use and emitting thinking events.
func (h *completionsHandler) appendThinking(text string) {
	if text == "" {
		return
	}
	if h.reasoningIdx < 0 {
		h.reasoningIdx = len(h.partial.Content)
		h.partial.Content = append(h.partial.Content, ContentBlock{Type: ContentTypeThinking})
		h.emit(AssistantMessageEvent{
			Type: EventThinkingStart, Partial: h.partial, ContentIndex: h.reasoningIdx,
		})
	}
	h.partial.Content[h.reasoningIdx].Thinking = blockAccum(h.accums, h.partial.Content, h.reasoningIdx).Append(text)
	h.emit(AssistantMessageEvent{
		Type: EventThinkingDelta, Partial: h.partial, ContentIndex: h.reasoningIdx, Delta: text,
	})
}

// closeThinking emits thinking_end once reasoning deltas stop flowing
// (the first content or tool-call delta marks the end of thinking).
func (h *completionsHandler) closeThinking() {
	if h.reasoningIdx >= 0 {
		h.emit(AssistantMessageEvent{
			Type: EventThinkingEnd, Partial: h.partial, ContentIndex: h.reasoningIdx,
		})
		h.reasoningIdx = -1
	}
}

func (h *completionsHandler) appendText(text string) {
	var idx int
	if len(h.partial.Content) == 0 || h.partial.Content[len(h.partial.Content)-1].Type != ContentTypeText {
		idx = len(h.partial.Content)
		h.partial.Content = append(h.partial.Content, ContentBlock{Type: ContentTypeText})
		h.emit(AssistantMessageEvent{
			Type: EventTextStart, Partial: h.partial, ContentIndex: idx,
		})
	} else {
		idx = len(h.partial.Content) - 1
	}
	h.partial.Content[idx].Text = blockAccum(h.accums, h.partial.Content, idx).Append(text)
	h.emit(AssistantMessageEvent{
		Type: EventTextDelta, Partial: h.partial, ContentIndex: idx, Delta: text,
	})
}

// applyToolCallDelta creates or updates a tool call block from a streaming
// delta, mirroring the index-keyed accumulation of chat completions. The
// first delta for an index usually carries id + name, later deltas only
// arguments; toolcall_end is emitted by finalizeToolCalls once the stream
// finishes, when all arguments have arrived.
func (h *completionsHandler) applyToolCallDelta(tc struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}) {
	idx, exists := h.toolIdx[tc.Index]
	if !exists {
		idx = len(h.partial.Content)
		// Note: construct the block directly so Arguments stays nil until
		// finalizeToolCalls (ToolCallBlock would normalize nil to {}).
		h.partial.Content = append(h.partial.Content, ContentBlock{
			Type: ContentTypeToolCall, ToolCallID: tc.ID,
		})
		h.toolIdx[tc.Index] = idx
		h.argsJSON[idx] = ""
		h.emit(AssistantMessageEvent{
			Type: EventToolCallStart, Partial: h.partial, ContentIndex: idx,
		})
	}
	block := &h.partial.Content[idx]
	if tc.ID != "" {
		block.ToolCallID = tc.ID
	}
	if tc.Function != nil {
		if tc.Function.Name != "" {
			block.ToolName = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			h.argsJSON[idx] += tc.Function.Arguments
			h.emit(AssistantMessageEvent{
				Type: EventToolCallDelta, Partial: h.partial, ContentIndex: idx,
				Delta: tc.Function.Arguments,
			})
		}
	}
}

// finalizeToolCalls parses the accumulated arguments of all tool calls and
// emits toolcall_end for each. Called after the stream terminates.
func (h *completionsHandler) finalizeToolCalls() {
	for _, idx := range h.toolIdx {
		block := &h.partial.Content[idx]
		if block.Type != ContentTypeToolCall || block.Arguments != nil {
			continue
		}
		args, err := parseArguments(h.argsJSON[idx])
		if err != nil {
			args = map[string]any{}
		}
		block.Arguments = args
		h.emit(AssistantMessageEvent{
			Type: EventToolCallEnd, Partial: h.partial, ContentIndex: idx,
			ToolCall: toolCallBlockCopy(*block),
		})
	}
}

func (h *completionsHandler) emitStart() {
	if h.emittedStart {
		return
	}
	h.emittedStart = true
	h.emit(AssistantMessageEvent{Type: EventStart, Partial: h.partial})
}
