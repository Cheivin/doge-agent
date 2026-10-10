package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func init() {
	RegisterAPIStream(APIOpenAIResponses, openAIResponsesStream)
}

// errStopSSE is returned from the SSE callback to stop iteration after a
// terminal response event was processed.
var errStopSSE = errors.New("stop sse iteration")

// openAIResponsesStream streams a request to the OpenAI Responses API
// (POST /responses), normalizing its SSE events into AssistantMessageEvents.
func openAIResponsesStream(model *Model, ctx *Context, opts *StreamOptions) *EventStream {
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
		resp, err := doStreamRequestRetry(streamCtx, model.BaseURL+"/responses", headers,
			func() ([]byte, error) { return buildResponsesBody(model, ctx, opts) })
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

		h := &responsesHandler{model: model, opts: opts, stream: s,
			partial: NewAssistantMessage(model.API, model.Provider, model.ID),
			textIdx: map[string]int{}, toolIdx: map[string]int{},
			reasoningIdx: map[string]int{}, argsJSON: map[string]string{},
			accums: map[int]*textAccum{}}
		err = IterateSSE(stallWrapped(resp.Body, streamCancel), h.handle)
		msg := h.partial
		// Terminal-state writes take the message lock: the partial may
		// already be marshaled by concurrent event consumers.
		h.partial.mu.Lock()
		switch {
		case errors.Is(err, errStopSSE):
			// Terminal state was recorded by the handler; emit and finish.
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
		h.partial.mu.Unlock()
		endStream(s, msg)
	}()
	return s
}

func requestCtx(opts *StreamOptions) context.Context {
	if opts != nil && opts.Context != nil {
		return opts.Context
	}
	return context.Background()
}

// requestFailureMessage maps a request error to a final message, honoring
// cancellation.
func requestFailureMessage(model *Model, opts *StreamOptions, err error) *Message {
	if requestCtx(opts).Err() != nil {
		return makeAbortedMessage(model)
	}
	return makeErrorMessage(model, StopReasonError, "request failed: "+err.Error())
}

// buildResponsesBody converts the unified context into an OpenAI Responses
// API request payload.
func buildResponsesBody(model *Model, ctx *Context, opts *StreamOptions) ([]byte, error) {
	body := map[string]any{
		"model":  model.ID,
		"input":  convertResponsesInput(ctx.Messages),
		"stream": true,
	}
	if ctx.SystemPrompt != "" {
		body["instructions"] = ctx.SystemPrompt
	}
	if len(ctx.Tools) > 0 {
		body["tools"] = convertResponsesTools(ctx.Tools)
	}
	if max := computeMaxTokens(model, ctx, opts); max > 0 {
		body["max_output_tokens"] = max
	}
	if effort := resolveResponsesReasoning(model, opts); effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	return json.Marshal(body)
}

func convertResponsesTools(tools []*Tool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		})
	}
	return out
}

// convertResponsesInput converts unified messages into Responses API input
// items. Tool results become user messages with function_call_output content;
// assistant tool calls become function_call content blocks.
//
// Consecutive tool results are merged into a single user message with
// multiple function_call_output blocks: some gateways reject one user
// message per tool result.
func convertResponsesInput(messages []*Message) []any {
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		switch m.Role {
		case RoleUser:
			out = append(out, map[string]any{"role": "user", "content": convertUserContent(m.Content)})
		case RoleAssistant:
			blocks := []any{}
			for _, b := range m.Content {
				switch b.Type {
				case ContentTypeText:
					block := map[string]any{"type": "output_text", "text": b.Text}
					if b.TextSignature != "" {
						block["text_signature"] = b.TextSignature
					}
					blocks = append(blocks, block)
				case ContentTypeToolCall:
					blocks = append(blocks, map[string]any{
						"type":      "function_call",
						"call_id":   b.ToolCallID,
						"name":      b.ToolName,
						"arguments": marshalJSONString(b.Arguments),
					})
				}
			}
			// A pure-abort marker (no text, no tool calls) carries no
			// information for the model and produces an invalid empty content
			// array — drop it from the request history.
			if len(blocks) == 0 {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case RoleToolResult:
			blocks := []any{}
			for i < len(messages) && messages[i].Role == RoleToolResult {
				tr := messages[i]
				blocks = append(blocks, map[string]any{
					"type":    "function_call_output",
					"call_id": tr.ToolCallID,
					"output":  toolResultOutput(tr),
				})
				i++
			}
			i-- // the loop increments past the last tool result
			out = append(out, map[string]any{"role": "user", "content": blocks})
		}
	}
	return out
}

func convertUserContent(content []ContentBlock) any {
	if len(content) == 1 && content[0].Type == ContentTypeText {
		return content[0].Text
	}
	blocks := []any{}
	for _, b := range content {
		switch b.Type {
		case ContentTypeText:
			blocks = append(blocks, map[string]any{"type": "input_text", "text": b.Text})
		case ContentTypeImage:
			blocks = append(blocks, map[string]any{
				"type":      "input_image",
				"image_url": "data:" + b.MimeType + ";base64," + base64Encode(b.Data),
				"detail":    "auto",
			})
		}
	}
	return blocks
}

// toolResultOutput renders a tool result as the string the API expects:
// the text for single-text results, otherwise a rendered blob.
func toolResultOutput(m *Message) string {
	if len(m.Content) == 1 && m.Content[0].Type == ContentTypeText {
		return m.Content[0].Text
	}
	var parts []string
	for _, b := range m.Content {
		switch b.Type {
		case ContentTypeText:
			parts = append(parts, b.Text)
		case ContentTypeImage:
			parts = append(parts, "data:"+b.MimeType+";base64,"+base64Encode(b.Data))
		}
	}
	return strings.Join(parts, "\n")
}

// resolveResponsesReasoning maps the unified thinking level to an OpenAI
// reasoning effort. Returns "" when reasoning is disabled.
func resolveResponsesReasoning(model *Model, opts *StreamOptions) string {
	if opts == nil || opts.Reasoning == "" || opts.Reasoning == "off" {
		return ""
	}
	if v, ok := model.ThinkingLevelMap[opts.Reasoning]; ok && v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	switch opts.Reasoning {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	default:
		return "high"
	}
}

// responsesHandler accumulates the streaming response into partial.
type responsesHandler struct {
	model *Model
	opts  *StreamOptions

	stream *EventStream

	partial      *Message
	emittedStart bool

	// item_id → content index of the text block
	textIdx map[string]int
	// item_id → content index of the tool call block
	toolIdx map[string]int
	// item_id → content index of the reasoning (thinking) block
	reasoningIdx map[string]int
	// item_id → accumulated raw tool call arguments JSON
	argsJSON map[string]string
	// accums holds the per-content-block text accumulators so streaming text
	// and thinking appends don't re-copy the whole block on every delta.
	accums map[int]*textAccum

	// pending batches the events of a locked mutation phase, flushed after
	// the lock is released (see emit/flush).
	pending []AssistantMessageEvent
}

func (h *responsesHandler) emit(evs ...AssistantMessageEvent) {
	h.pending = append(h.pending, evs...)
}

func (h *responsesHandler) flush() {
	for _, ev := range h.pending {
		h.stream.Push(ev)
	}
	h.pending = h.pending[:0]
}

func (h *responsesHandler) handle(ev SSEEvent) error {
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

func (h *responsesHandler) handleLocked(ev SSEEvent) error {
	switch {
	case ev.Event == "response.created":
		var r struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		_ = json.Unmarshal(ev.Data, &r)
		h.partial.ResponseID = r.Response.ID
		h.emitStart()

	case ev.Event == "response.output_item.added":
		var r struct {
			Item struct {
				ID        string `json:"id"`
				Type      string `json:"type"`
				Name      string `json:"name"`
				Arguments any    `json:"arguments"`
			} `json:"item"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		h.emitStart()
		if r.Item.Type == "reasoning" {
			// Reasoning item: exposes the model's thinking as a thinking
			// block (visible in the UI, not sent back to the provider).
			idx := len(h.partial.Content)
			h.partial.Content = append(h.partial.Content, ContentBlock{Type: ContentTypeThinking})
			h.reasoningIdx[r.Item.ID] = idx
			h.emit(AssistantMessageEvent{
				Type: EventThinkingStart, Partial: h.partial, ContentIndex: idx,
			})
			return nil
		}
		if r.Item.Type != "function_call" {
			return nil
		}
		idx := len(h.partial.Content)
		h.partial.Content = append(h.partial.Content, ToolCallBlock(r.Item.ID, r.Item.Name, nil))
		h.toolIdx[r.Item.ID] = idx
		// Some providers send the full arguments object on item add; keep it
		// for finalization (arguments string or object both work).
		switch a := r.Item.Arguments.(type) {
		case string:
			h.argsJSON[r.Item.ID] = a
		case map[string]any:
			if len(a) > 0 {
				if data, err := json.Marshal(a); err == nil {
					h.argsJSON[r.Item.ID] = string(data)
				} else {
					h.argsJSON[r.Item.ID] = ""
				}
			} else {
				h.argsJSON[r.Item.ID] = ""
			}
		default:
			h.argsJSON[r.Item.ID] = ""
		}
		h.emit(AssistantMessageEvent{
			Type: EventToolCallStart, Partial: h.partial, ContentIndex: idx,
		})

	case ev.Event == "response.content_part.added":
		var r struct {
			ItemID string `json:"item_id"`
			Part   struct {
				Type string `json:"type"`
			} `json:"part"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		if r.Part.Type != "output_text" {
			return nil
		}
		idx := len(h.partial.Content)
		h.partial.Content = append(h.partial.Content, ContentBlock{Type: ContentTypeText})
		h.textIdx[r.ItemID] = idx
		h.emit(AssistantMessageEvent{
			Type: EventTextStart, Partial: h.partial, ContentIndex: idx,
		})

	case ev.Event == "response.output_text.delta":
		var r struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		idx, ok := h.textIdx[r.ItemID]
		if !ok {
			idx = h.lastTextIndex()
		}
		if idx < 0 {
			return nil
		}
		h.partial.Content[idx].Text = blockAccum(h.accums, h.partial.Content, idx).Append(r.Delta)
		h.emit(AssistantMessageEvent{
			Type: EventTextDelta, Partial: h.partial, ContentIndex: idx, Delta: r.Delta,
		})

	case ev.Event == "response.output_text.done":
		var r struct {
			ItemID        string `json:"item_id"`
			Text          string `json:"text"`
			TextSignature string `json:"text_signature"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		idx, ok := h.textIdx[r.ItemID]
		if !ok {
			idx = h.lastTextIndex()
		}
		if idx >= 0 {
			if h.partial.Content[idx].Text == "" {
				h.partial.Content[idx].Text = blockAccum(h.accums, h.partial.Content, idx).Replace(r.Text)
			}
			h.partial.Content[idx].TextSignature = r.TextSignature
			h.emit(AssistantMessageEvent{
				Type: EventTextEnd, Partial: h.partial, ContentIndex: idx,
			})
		}

	case ev.Event == "response.reasoning_summary_text.delta" || ev.Event == "response.reasoning_text.delta":
		var r struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		idx, ok := h.reasoningIdx[r.ItemID]
		if !ok {
			return nil
		}
		h.partial.Content[idx].Thinking = blockAccum(h.accums, h.partial.Content, idx).Append(r.Delta)
		h.emit(AssistantMessageEvent{
			Type: EventThinkingDelta, Partial: h.partial, ContentIndex: idx, Delta: r.Delta,
		})

	case ev.Event == "response.function_call_arguments.delta":
		var r struct {
			ItemID string `json:"item_id"`
			Delta  string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		idx, ok := h.toolIdx[r.ItemID]
		if !ok {
			return nil
		}
		h.argsJSON[r.ItemID] += r.Delta
		h.emit(AssistantMessageEvent{
			Type: EventToolCallDelta, Partial: h.partial, ContentIndex: idx, Delta: r.Delta,
		})

	case ev.Event == "response.function_call_arguments.done" || ev.Event == "response.output_item.done":
		h.finalizeToolCall(ev.Data)

	case ev.Event == "response.completed" || ev.Event == "response.incomplete":
		return h.handleCompleted(ev)

	case ev.Event == "response.failed":
		var r struct {
			Response struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		_ = json.Unmarshal(ev.Data, &r)
		h.partial.StopReason = StopReasonError
		msg := r.Response.Error.Message
		if r.Response.Error.Code != "" {
			msg = r.Response.Error.Code + ": " + msg
		}
		h.partial.ErrorMessage = msg
		return errStopSSE

	case ev.Event == "error":
		var r struct {
			Error struct {
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

// handleCompleted processes response.completed / response.incomplete and
// derives the stop reason.
func (h *responsesHandler) handleCompleted(ev SSEEvent) error {
	var r struct {
		Response struct {
			ID                string `json:"id"`
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
				InputDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputDetails *struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(ev.Data, &r); err != nil {
		return err
	}
	if r.Response.ID != "" {
		h.partial.ResponseID = r.Response.ID
	}
	if u := r.Response.Usage; u != nil {
		usage := &Usage{
			Input:       u.InputTokens,
			Output:      u.OutputTokens,
			TotalTokens: u.TotalTokens,
		}
		if u.InputDetails != nil {
			usage.CacheRead = u.InputDetails.CachedTokens
		}
		if u.OutputDetails != nil {
			usage.Reasoning = u.OutputDetails.ReasoningTokens
		}
		usage.Cost = CalculateCost(*usage, h.model.Cost)
		h.partial.Usage = usage
	}
	switch r.Response.Status {
	case "incomplete":
		if r.Response.IncompleteDetails != nil && r.Response.IncompleteDetails.Reason == "max_output_tokens" {
			h.partial.StopReason = StopReasonLength
		} else {
			h.partial.StopReason = StopReasonStop
		}
	case "failed":
		h.partial.StopReason = StopReasonError
		h.partial.ErrorMessage = "response failed"
	default:
		if h.hasToolCalls() {
			h.partial.StopReason = StopReasonToolUse
		} else {
			h.partial.StopReason = StopReasonStop
		}
	}
	return errStopSSE
}

// finalizeToolCall parses the accumulated arguments JSON for the item in the
// event data and emits toolcall_end. Reasoning items are finalized here too.
func (h *responsesHandler) finalizeToolCall(data []byte) {
	if h.finalizeReasoningItem(data) {
		return
	}
	var r struct {
		ItemID    string `json:"item_id"`
		Arguments any    `json:"arguments"`
		Item      *struct {
			ID        string `json:"id"`
			Arguments any    `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return
	}
	itemID := r.ItemID
	if itemID == "" && r.Item != nil {
		itemID = r.Item.ID
	}
	idx, ok := h.toolIdx[itemID]
	if !ok {
		return
	}
	// Normalize the arguments: string JSON or an already-parsed object.
	raw := argumentsToString(r.Arguments)
	if raw == "" {
		raw = h.argsJSON[itemID]
	}
	args, err := parseArguments(raw)
	if err != nil {
		h.partial.Content[idx].Arguments = map[string]any{}
	} else {
		h.partial.Content[idx].Arguments = args
	}
	h.emit(AssistantMessageEvent{
		Type: EventToolCallEnd, Partial: h.partial, ContentIndex: idx,
		ToolCall: toolCallBlockCopy(h.partial.Content[idx]),
	})
}

// argumentsToString renders raw tool-call arguments as a JSON string,
// accepting both a JSON string and an already-parsed object.
func argumentsToString(args any) string {
	switch a := args.(type) {
	case string:
		return a
	case map[string]any:
		if len(a) == 0 {
			return ""
		}
		if data, err := json.Marshal(a); err == nil {
			return string(data)
		}
	}
	return ""
}

// finalizeReasoningItem finalizes a reasoning item from output_item.done:
// fills the thinking block from the item content and emits thinking_end.
func (h *responsesHandler) finalizeReasoningItem(data []byte) bool {
	var r struct {
		Item *struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Item == nil {
		return false
	}
	if r.Item.Type != "reasoning" {
		return false
	}
	idx, ok := h.reasoningIdx[r.Item.ID]
	if !ok {
		return false
	}
	// The done event carries the final reasoning text; replace the streamed
	// summary with it to avoid duplicating the delta text.
	var final strings.Builder
	filled := false
	for _, part := range r.Item.Content {
		if part.Type == "reasoning_text" && part.Text != "" {
			if !filled {
				final.Reset()
				filled = true
			}
			final.WriteString(part.Text)
		}
	}
	if filled {
		h.partial.Content[idx].Thinking = blockAccum(h.accums, h.partial.Content, idx).Replace(final.String())
	}
	h.emit(AssistantMessageEvent{
		Type: EventThinkingEnd, Partial: h.partial, ContentIndex: idx,
	})
	return true
}

func (h *responsesHandler) emitStart() {
	if h.emittedStart {
		return
	}
	h.emittedStart = true
	h.emit(AssistantMessageEvent{Type: EventStart, Partial: h.partial})
}

func (h *responsesHandler) lastTextIndex() int {
	best := -1
	for _, idx := range h.textIdx {
		if idx > best {
			best = idx
		}
	}
	return best
}

func (h *responsesHandler) hasToolCalls() bool {
	for _, b := range h.partial.Content {
		if b.Type == ContentTypeToolCall {
			return true
		}
	}
	return false
}

// parseArguments parses a raw tool-call arguments JSON string.
func parseArguments(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	if args == nil {
		return map[string]any{}, nil
	}
	return args, nil
}

func toolCallBlockCopy(b ContentBlock) *ContentBlock {
	c := b
	return &c
}

// marshalJSONString marshals v as a JSON string for API payloads.
func marshalJSONString(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// base64Encode encodes bytes as base64.
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// computeMaxTokens derives the output token budget: explicit option first,
// then the model's max, clamped to the context window minus an estimate of
// the input and a safety margin.
func computeMaxTokens(model *Model, ctx *Context, opts *StreamOptions) int {
	if opts != nil && opts.MaxTokens > 0 {
		return opts.MaxTokens
	}
	max := model.MaxTokens
	if max <= 0 {
		max = 8192
	}
	if model.ContextWindow > 0 {
		available := model.ContextWindow - EstimateTokens(ctx.Messages) - 4096
		if available < 1024 {
			available = 1024
		}
		if available < max {
			max = available
		}
	}
	return max
}
