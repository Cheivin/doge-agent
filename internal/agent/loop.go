package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"doge/internal/ai"
	"doge/pkg/logx"
)

// loopConfig carries per-run configuration into the loop.
type loopConfig struct {
	model         *ai.Model
	thinkingLevel string
	sessionID     string

	streamFn      StreamFn
	toolExecution ToolExecutionMode

	beforeToolCall  func(ctx BeforeToolCallContext) (*BeforeToolCallResult, error)
	afterToolCall   func(ctx AfterToolCallContext) (*AfterToolCallResult, error)
	prepareNextTurn func(ctx *TurnContext) (*TurnUpdate, error)
	getAPIKey       func(provider string) string

	getSteeringMessages func() []*ai.Message
	getFollowUpMessages func() []*ai.Message
	// steeringAvailable peeks the steering queue without draining, so a tool
	// batch can interrupt itself mid-flight when a new user message arrives.
	steeringAvailable func() bool
}

// runLoop runs the agent loop with new prompt messages. It implements the
// runAgentLoop: the inner loop processes tool calls and steering messages,
// the outer loop continues when follow-up messages arrive.
func runLoop(prompts []*ai.Message, context *AgentContext, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) error {
	newMessages := append([]*ai.Message{}, prompts...)
	current := &AgentContext{
		SystemPrompt: context.SystemPrompt,
		Messages:     append(append([]*ai.Message{}, context.Messages...), prompts...),
		Tools:        context.Tools,
	}

	if err := emit(AgentEvent{Type: EventAgentStart}); err != nil {
		return err
	}
	if err := emit(AgentEvent{Type: EventTurnStart}); err != nil {
		return err
	}
	for _, p := range prompts {
		if err := emit(AgentEvent{Type: EventMessageStart, Message: p}); err != nil {
			return err
		}
		if err := emit(AgentEvent{Type: EventMessageEnd, Message: p}); err != nil {
			return err
		}
	}
	return runLoopBody(current, newMessages, config, emit, ctx)
}

// runLoopContinue resumes from the existing transcript without adding a
// message.
func runLoopContinue(context *AgentContext, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) error {
	if len(context.Messages) == 0 {
		return errors.New("cannot continue: no messages in context")
	}
	if context.Messages[len(context.Messages)-1].Role == ai.RoleAssistant {
		return errors.New("cannot continue from message role: assistant")
	}
	newMessages := []*ai.Message{}
	current := &AgentContext{
		SystemPrompt: context.SystemPrompt,
		Messages:     append([]*ai.Message{}, context.Messages...),
		Tools:        context.Tools,
	}
	if err := emit(AgentEvent{Type: EventAgentStart}); err != nil {
		return err
	}
	if err := emit(AgentEvent{Type: EventTurnStart}); err != nil {
		return err
	}
	return runLoopBody(current, newMessages, config, emit, ctx)
}

// runLoopBody is the main loop shared by runLoop and runLoopContinue.
func runLoopBody(current *AgentContext, newMessages []*ai.Message, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) error {
	firstTurn := true
	started := time.Now()
	if logx.Enabled(logx.LevelInfo) {
		model := ""
		if config.model != nil {
			model = config.model.Provider + "/" + config.model.ID
		}
		logx.Infof("[agent] run start session=%s model=%s messages=%d", config.sessionID, model, len(current.Messages))
	}
	// Deferred args are evaluated at defer time in Go, so the duration must
	// be computed inside a closure to see the real elapsed time.
	defer func() {
		logx.Infof("[agent] run end session=%s duration=%s messages=%d", config.sessionID, time.Since(started).Round(time.Millisecond), len(current.Messages))
	}()
	// Check for steering messages at start (user may have typed while waiting).
	pending := config.getSteeringMessages()

	for {
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pending) > 0 {
			if !firstTurn {
				if err := emit(AgentEvent{Type: EventTurnStart}); err != nil {
					return err
				}
			}
			firstTurn = false
			logx.Debugf("[agent] turn start")

			// Inject pending messages before the next assistant response.
			if len(pending) > 0 {
				for _, m := range pending {
					if err := emit(AgentEvent{Type: EventMessageStart, Message: m}); err != nil {
						return err
					}
					if err := emit(AgentEvent{Type: EventMessageEnd, Message: m}); err != nil {
						return err
					}
					current.Messages = append(current.Messages, m)
					newMessages = append(newMessages, m)
				}
				pending = nil
			}

			// Stream the assistant response.
			message, err := streamAssistantResponse(current, config, emit, ctx)
			if err != nil {
				logx.Warnf("[agent] stream aborted by error: %v", err)
				return err
			}
			newMessages = append(newMessages, message)

			if message.StopReason == ai.StopReasonError || message.StopReason == ai.StopReasonAborted {
				logx.Warnf("[agent] stream ended reason=%s error=%q", message.StopReason, message.ErrorMessage)
				if err := emit(AgentEvent{Type: EventTurnEnd, Message: message}); err != nil {
					return err
				}
				return emit(AgentEvent{Type: EventAgentEnd, Messages: newMessages})
			}

			// Execute tool calls.
			var toolResults []*ai.Message
			hasMoreToolCalls = false
			if calls := toolCallsOf(message); len(calls) > 0 {
				var executed toolBatch
				if message.StopReason == ai.StopReasonLength {
					// Output hit the token limit: every tool call may carry
					// truncated arguments. Fail them all instead of executing
					// potentially borked calls.
					executed = failTruncatedToolCalls(calls, emit)
				} else {
					executed = executeToolCalls(current, message, config, emit, ctx)
				}
				toolResults = executed.messages
				hasMoreToolCalls = !executed.terminate
				for _, r := range toolResults {
					current.Messages = append(current.Messages, r)
					newMessages = append(newMessages, r)
				}
			}

			if err := emit(AgentEvent{Type: EventTurnEnd, Message: message, ToolResults: toolResults}); err != nil {
				return err
			}

			if config.prepareNextTurn != nil {
				update, err := config.prepareNextTurn(&TurnContext{
					Message:     message,
					ToolResults: toolResults,
					Context:     current,
					NewMessages: newMessages,
				})
				if err != nil {
					return err
				}
				if update != nil {
					if update.Context != nil {
						current = update.Context
					}
					if update.Model != nil {
						config.model = update.Model
					}
					if update.ThinkingLevel != nil {
						config.thinkingLevel = *update.ThinkingLevel
					}
				}
			}

			pending = config.getSteeringMessages()
			if len(pending) > 0 {
				logx.Debugf("[agent] steering messages injected: %d", len(pending))
			}
		}

		// The agent would stop here; check for follow-up messages.
		followUps := config.getFollowUpMessages()
		if len(followUps) > 0 {
			logx.Debugf("[agent] follow-up messages injected: %d", len(followUps))
			pending = followUps
			continue
		}
		break
	}

	return emit(AgentEvent{Type: EventAgentEnd, Messages: newMessages})
}

// streamAssistantResponse streams one assistant response, emitting message
// lifecycle events and appending the message to the context.
func streamAssistantResponse(current *AgentContext, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) (*ai.Message, error) {
	model := config.model
	if model == nil {
		return aiMessageError("no model selected"), nil
	}

	llmCtx := &ai.Context{
		SystemPrompt: current.SystemPrompt,
		Messages:     stripOrphanedToolCalls(current.Messages),
		Tools:        toolsToLlm(current.Tools),
	}

	apiKey := ""
	if config.getAPIKey != nil {
		apiKey = config.getAPIKey(model.Provider)
	}

	stream := config.streamFn(model, llmCtx, &ai.StreamOptions{
		APIKey:    apiKey,
		Reasoning: config.thinkingLevel,
		SessionID: config.sessionID,
		Context:   ctx,
	})

	var added bool
	var streamErr error
	err := stream.Range(func(ev ai.AssistantMessageEvent) error {
		switch ev.Type {
		case ai.EventStart:
			current.Messages = append(current.Messages, ev.Partial)
			added = true
			return emit(AgentEvent{Type: EventMessageStart, Message: ev.Partial})
		case ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd,
			ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingEnd,
			ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd:
			ae := ev
			current.Messages[len(current.Messages)-1] = ev.Partial
			return emit(AgentEvent{
				Type:                  EventMessageUpdate,
				Message:               ev.Partial,
				AssistantMessageEvent: &ae,
			})
		case ai.EventDone, ai.EventError:
			return errStopStream
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopStream) {
		streamErr = err
	}

	final, resultErr := stream.Result()
	if resultErr != nil {
		return aiMessageError("stream failed: " + resultErr.Error()), nil
	}
	if streamErr != nil {
		// The consumer aborted (listener error or context); propagate.
		return final, streamErr
	}

	if added {
		current.Messages[len(current.Messages)-1] = final
	} else {
		current.Messages = append(current.Messages, final)
		if err := emit(AgentEvent{Type: EventMessageStart, Message: final}); err != nil {
			return final, err
		}
	}
	if err := emit(AgentEvent{Type: EventMessageEnd, Message: final}); err != nil {
		return final, err
	}
	return final, nil
}

var errStopStream = errors.New("stop stream iteration")

// toolBatch is the outcome of executing the tool calls of one message.
type toolBatch struct {
	messages  []*ai.Message
	terminate bool
}

// executeToolCalls prepares and executes the tool calls of an assistant
// message, emitting tool lifecycle events and producing tool result messages
// in assistant source order.
func executeToolCalls(current *AgentContext, assistant *ai.Message, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) toolBatch {
	calls := toolCallsOf(assistant)

	// Sequential mode if configured or any tool requires it.
	sequential := config.toolExecution == ToolExecutionSequential
	for _, c := range calls {
		if tool := findTool(current.Tools, c.ToolName); tool != nil && tool.ExecutionMode == ToolExecutionSequential {
			sequential = true
		}
	}
	if sequential {
		return executeToolCallsSequential(current, assistant, calls, config, emit, ctx)
	}
	return executeToolCallsParallel(current, assistant, calls, config, emit, ctx)
}

type finalizedToolCall struct {
	toolCall ai.ContentBlock
	result   *AgentToolResult
	isError  bool
}

func executeToolCallsSequential(current *AgentContext, assistant *ai.Message, calls []ai.ContentBlock, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) toolBatch {
	var messages []*ai.Message
	var finalized []finalizedToolCall
	for _, call := range calls {
		if err := emit(AgentEvent{Type: EventToolExecutionStart, ToolCallID: call.ToolCallID, ToolName: call.ToolName, Args: call.Arguments}); err != nil {
			return toolBatch{messages: messages, terminate: true}
		}
		// A steering message queued while the previous tool ran skips the
		// remaining tools (mirroring the parallel batch interrupt).
		if config.steeringAvailable != nil && config.steeringAvailable() {
			f := skippedToolResult(call, "a new user message arrived")
			_ = emitToolExecutionEnd(f, emit)
			finalized = append(finalized, f)
			tr := toolResultMessage(f)
			if err := emitMessagePair(emit, tr); err != nil {
				return toolBatch{messages: messages, terminate: true}
			}
			messages = append(messages, tr)
			continue
		}
		f, err := runOneToolCall(current, assistant, call, config, emit, ctx)
		if err != nil {
			return toolBatch{messages: messages, terminate: true}
		}
		finalized = append(finalized, f)
		tr := toolResultMessage(f)
		if err := emitMessagePair(emit, tr); err != nil {
			return toolBatch{messages: messages, terminate: true}
		}
		messages = append(messages, tr)
		if ctx.Err() != nil {
			break
		}
	}
	return toolBatch{messages: messages, terminate: shouldTerminate(finalized)}
}

// steeringPollInterval is how often the parallel batch checks the steering
// queue while tools run, so a mid-batch steering message interrupts the batch
// within one tick instead of waiting for the whole batch to drain.
const steeringPollInterval = 10 * time.Millisecond

func executeToolCallsParallel(current *AgentContext, assistant *ai.Message, calls []ai.ContentBlock, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) toolBatch {
	// Preflight sequentially (validation + beforeToolCall), then run the
	// allowed tools concurrently; report results in source order.
	type entry struct {
		call     ai.ContentBlock
		finalize func(toolCtx context.Context) (finalizedToolCall, error)
	}
	entries := make([]entry, 0, len(calls))
	for _, call := range calls {
		if err := emit(AgentEvent{Type: EventToolExecutionStart, ToolCallID: call.ToolCallID, ToolName: call.ToolName, Args: call.Arguments}); err != nil {
			return toolBatch{terminate: true}
		}
		prepared, immediate, err := prepareToolCall(current, assistant, call, config, ctx)
		if err != nil {
			return toolBatch{terminate: true}
		}
		if immediate != nil {
			if err := emitToolExecutionEnd(*immediate, emit); err != nil {
				return toolBatch{terminate: true}
			}
			f := *immediate
			entries = append(entries, entry{call: call, finalize: func(context.Context) (finalizedToolCall, error) { return f, nil }})
			continue
		}
		entries = append(entries, entry{call: call, finalize: func(toolCtx context.Context) (finalizedToolCall, error) {
			return runPreparedToolCall(current, assistant, prepared, config, emit, toolCtx)
		}})
	}

	// Batch-scoped context so a steering message (or a global abort) cancels
	// every in-flight tool at its next ctx checkpoint instead of waiting for
	// the whole batch to drain. Tools that never got admitted are reported as
	// skipped; tools admitted mid-flight are reported as interrupted.
	toolCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	results := make([]finalizedToolCall, len(entries))
	started := make([]bool, len(entries))  // admitted into finalize
	finished := make([]bool, len(entries)) // wrote a result
	var mu sync.Mutex
	interrupted := false
	stopReason := ""

	// tryStart admits tool i into execution; it fails once the batch has been
	// interrupted, returning the reason so the tool can be reported as skipped.
	tryStart := func(i int) (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		if interrupted {
			return false, stopReason
		}
		started[i] = true
		return true, ""
	}
	finish := func(i int, f finalizedToolCall) {
		mu.Lock()
		results[i] = f
		finished[i] = true
		mu.Unlock()
	}
	interrupt := func(reason string) {
		mu.Lock()
		interrupted = true
		stopReason = reason
		mu.Unlock()
	}

	for i, e := range entries {
		wg.Add(1)
		go func(i int, e entry) {
			defer wg.Done()
			ok, reason := tryStart(i)
			if !ok {
				f := skippedToolResult(e.call, reason)
				_ = emitToolExecutionEnd(f, emit)
				finish(i, f)
				return
			}
			f, err := e.finalize(toolCtx)
			if err != nil {
				f = finalizedToolCall{toolCall: e.call, result: &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("tool execution failed: " + err.Error())}}, isError: true}
			}
			finish(i, f)
		}(i, e)
	}

	// Wait for the batch, polling for a steering message. On steering (or a
	// global abort) cancel the batch, snapshot which tools already finished,
	// and wait for the in-flight ones to observe the cancellation.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	ticker := time.NewTicker(steeringPollInterval)
	defer ticker.Stop()
	var doneBeforeCancel []bool
waitLoop:
	for {
		select {
		case <-done:
			break waitLoop
		case <-ctx.Done():
			interrupt("the run was aborted")
			cancel()
			mu.Lock()
			doneBeforeCancel = append([]bool{}, finished...)
			mu.Unlock()
			<-done
			break waitLoop
		case <-ticker.C:
			if config.steeringAvailable != nil && config.steeringAvailable() {
				interrupt("a new user message arrived")
				cancel()
				mu.Lock()
				doneBeforeCancel = append([]bool{}, finished...)
				mu.Unlock()
				<-done
				break waitLoop
			}
		}
	}

	// Tools that were still running (or never admitted) when the batch was
	// interrupted are reported to the model as interrupted or skipped, so it
	// knows exactly what ran and what did not.
	if doneBeforeCancel != nil {
		logx.Warnf("[agent] tool batch interrupted: %s", stopReason)
		for i := range results {
			if doneBeforeCancel[i] {
				continue
			}
			if started[i] {
				results[i] = interruptedToolResult(entries[i].call, stopReason)
			} else {
				results[i] = skippedToolResult(entries[i].call, stopReason)
			}
		}
	}

	var messages []*ai.Message
	var finalized []finalizedToolCall
	for _, f := range results {
		finalized = append(finalized, f)
		tr := toolResultMessage(f)
		if err := emitMessagePair(emit, tr); err != nil {
			return toolBatch{messages: messages, terminate: true}
		}
		messages = append(messages, tr)
	}
	return toolBatch{messages: messages, terminate: shouldTerminate(finalized)}
}

// skippedToolResult reports a tool that never got to run because the batch
// was interrupted (a new user message arrived or the run was aborted).
func skippedToolResult(call ai.ContentBlock, reason string) finalizedToolCall {
	return finalizedToolCall{
		toolCall: call,
		result: &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(
			"Tool " + call.ToolName + " was not run because " + reason + ".")}},
		isError: true,
	}
}

// interruptedToolResult reports a tool that was running when the batch was
// interrupted; its side effects may be partial.
func interruptedToolResult(call ai.ContentBlock, reason string) finalizedToolCall {
	return finalizedToolCall{
		toolCall: call,
		result: &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(
			"Tool " + call.ToolName + " was interrupted before completing because " + reason + "; it may have had partial effects.")}},
		isError: true,
	}
}

// runOneToolCall prepares, executes, and finalizes a single tool call
// (sequential path).
func runOneToolCall(current *AgentContext, assistant *ai.Message, call ai.ContentBlock, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) (finalizedToolCall, error) {
	prepared, immediate, err := prepareToolCall(current, assistant, call, config, ctx)
	if err != nil {
		return finalizedToolCall{}, err
	}
	var f finalizedToolCall
	if immediate != nil {
		f = *immediate
	} else {
		f, err = runPreparedToolCall(current, assistant, prepared, config, emit, ctx)
		if err != nil {
			return finalizedToolCall{}, err
		}
	}
	if err := emitToolExecutionEnd(f, emit); err != nil {
		return finalizedToolCall{}, err
	}
	return f, nil
}

// runPreparedToolCall executes an already-prepared tool call and applies the
// afterToolCall hook.
func runPreparedToolCall(current *AgentContext, assistant *ai.Message, prepared *preparedToolCall, config *loopConfig, emit func(AgentEvent) error, ctx context.Context) (finalizedToolCall, error) {
	logx.Debugf("[agent] tool %s start args=%s", prepared.call.ToolName, shortJSON(prepared.args))
	started := time.Now()
	result, isError := executePreparedToolCall(prepared, emit, ctx)

	if config.afterToolCall != nil {
		after, err := config.afterToolCall(AfterToolCallContext{
			AssistantMessage: assistant,
			ToolCall:         prepared.call,
			Args:             prepared.args,
			Result:           result,
			IsError:          isError,
			Context:          current,
		})
		if err != nil {
			result = &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("tool finalization failed: " + err.Error())}}
			isError = true
		} else if after != nil {
			if after.Content != nil {
				result.Content = after.Content
			}
			if after.Details != nil {
				result.Details = after.Details
			}
			if after.IsError != nil {
				isError = *after.IsError
			}
			if after.Terminate != nil {
				result.Terminate = *after.Terminate
			}
		}
	}

	f := finalizedToolCall{toolCall: prepared.call, result: result, isError: isError}
	logx.Infof("[agent] tool %s end %s error=%v", prepared.call.ToolName, time.Since(started).Round(time.Millisecond), isError)
	if err := emitToolExecutionEnd(f, emit); err != nil {
		return finalizedToolCall{}, err
	}
	return f, nil
}

func emitToolExecutionEnd(f finalizedToolCall, emit func(AgentEvent) error) error {
	return emit(AgentEvent{
		Type: EventToolExecutionEnd, ToolCallID: f.toolCall.ToolCallID, ToolName: f.toolCall.ToolName,
		Result: f.result, IsError: f.isError,
	})
}

// preparedToolCall is a tool call that passed validation and preflight.
type preparedToolCall struct {
	call ai.ContentBlock
	tool *AgentTool
	args map[string]any
}

// prepareToolCall validates the call, applies prepareArguments and the
// beforeToolCall hook. Returns (prepared, nil) for execution, or
// (nil, immediate) for immediate outcomes (blocked, invalid, missing tool).
func prepareToolCall(current *AgentContext, assistant *ai.Message, call ai.ContentBlock, config *loopConfig, ctx context.Context) (*preparedToolCall, *finalizedToolCall, error) {
	tool := findTool(current.Tools, call.ToolName)
	if tool == nil {
		logx.Warnf("[agent] tool %s not found", call.ToolName)
		return nil, &finalizedToolCall{
			toolCall: call,
			result:   &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("Tool " + call.ToolName + " not found")}},
			isError:  true,
		}, nil
	}

	rawArgs := call.Arguments
	if tool.PrepareArguments != nil {
		prepared, err := tool.PrepareArguments(rawArgs)
		if err != nil {
			return nil, &finalizedToolCall{
				toolCall: call,
				result:   &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("Invalid arguments: " + err.Error())}},
				isError:  true,
			}, nil
		}
		rawArgs = prepared
	}
	args, err := ai.ValidateToolArguments(&ai.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters}, rawArgs)
	if err != nil {
		return nil, &finalizedToolCall{
			toolCall: call,
			result:   &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("Invalid arguments: " + err.Error())}},
			isError:  true,
		}, nil
	}
	argsMap, _ := args.(map[string]any)
	if argsMap == nil {
		return nil, &finalizedToolCall{
			toolCall: call,
			result:   &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("Invalid arguments: expected an object")}},
			isError:  true,
		}, nil
	}

	if config.beforeToolCall != nil {
		before, err := config.beforeToolCall(BeforeToolCallContext{
			AssistantMessage: assistant, ToolCall: call, Args: argsMap, Context: current,
		})
		if err != nil {
			return nil, nil, err
		}
		if before != nil && before.Block {
			reason := before.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return nil, &finalizedToolCall{
				toolCall: call,
				result:   &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(reason)}},
				isError:  true,
			}, nil
		}
	}
	if ctx.Err() != nil {
		return nil, &finalizedToolCall{
			toolCall: call,
			result:   &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock("Operation aborted")}},
			isError:  true,
		}, nil
	}
	return &preparedToolCall{call: call, tool: tool, args: argsMap}, nil, nil
}

// executePreparedToolCall runs the tool, forwarding partial updates as
// tool_execution_update events. A tool error becomes an error result.
func executePreparedToolCall(prepared *preparedToolCall, emit func(AgentEvent) error, ctx context.Context) (*AgentToolResult, bool) {
	var mu sync.Mutex
	accepting := true
	updates := []func(){}

	name := prepared.call.ToolName
	started := time.Now()
	result, err := prepared.tool.Execute(prepared.call.ToolCallID, prepared.args, ctx, func(partial AgentToolResult) {
		mu.Lock()
		defer mu.Unlock()
		if !accepting {
			return
		}
		p := partial
		updates = append(updates, func() {
			_ = emit(AgentEvent{
				Type: EventToolExecutionUpdate, ToolCallID: prepared.call.ToolCallID,
				ToolName: prepared.call.ToolName, Args: prepared.call.Arguments,
				PartialResult: p,
			})
		})
	})
	mu.Lock()
	accepting = false
	pending := updates
	updates = nil
	mu.Unlock()
	for _, u := range pending {
		u()
	}
	if err != nil {
		logx.Warnf("[agent] tool %s end ERROR %s: %v", name, time.Since(started).Round(time.Millisecond), err)
		return &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(err.Error())}}, true
	}
	if result == nil {
		result = &AgentToolResult{}
	}
	return result, false
}

// failTruncatedToolCalls reports every tool call of a truncated message as an
// error without executing it (stopReason "length").
func failTruncatedToolCalls(calls []ai.ContentBlock, emit func(AgentEvent) error) toolBatch {
	var messages []*ai.Message
	for _, call := range calls {
		_ = emit(AgentEvent{Type: EventToolExecutionStart, ToolCallID: call.ToolCallID, ToolName: call.ToolName, Args: call.Arguments})
		f := finalizedToolCall{
			toolCall: call,
			result: &AgentToolResult{Content: []ai.ContentBlock{ai.TextBlock(
				`Tool call "` + call.ToolName + `" was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.`)},
			},
			isError: true,
		}
		_ = emitToolExecutionEnd(f, emit)
		tr := toolResultMessage(f)
		_ = emitMessagePair(emit, tr)
		messages = append(messages, tr)
	}
	return toolBatch{messages: messages}
}

// emitMessagePair emits message_start and message_end for a transcript
// message (tool results, injected user messages).
func emitMessagePair(emit func(AgentEvent) error, m *ai.Message) error {
	if err := emit(AgentEvent{Type: EventMessageStart, Message: m}); err != nil {
		return err
	}
	return emit(AgentEvent{Type: EventMessageEnd, Message: m})
}

func shouldTerminate(finalized []finalizedToolCall) bool {
	if len(finalized) == 0 {
		return false
	}
	for _, f := range finalized {
		if f.result == nil || !f.result.Terminate {
			return false
		}
	}
	return true
}

// toolResultMessage builds the toolResult message persisted to the
// transcript.
func toolResultMessage(f finalizedToolCall) *ai.Message {
	m := &ai.Message{
		Role:       ai.RoleToolResult,
		ToolCallID: f.toolCall.ToolCallID,
		ToolName:   f.toolCall.ToolName,
		IsError:    f.isError,
		Timestamp:  time.Now().UnixMilli(),
	}
	if f.result != nil {
		m.Content = f.result.Content
		m.Details = f.result.Details
		m.Usage = f.result.Usage
		if len(f.result.AddedToolNames) > 0 {
			m.AddedToolNames = f.result.AddedToolNames
		}
	}
	if m.Content == nil {
		m.Content = []ai.ContentBlock{}
	}
	return m
}

func toolCallsOf(m *ai.Message) []ai.ContentBlock {
	var calls []ai.ContentBlock
	for _, b := range m.Content {
		if b.Type == ai.ContentTypeToolCall {
			calls = append(calls, b)
		}
	}
	return calls
}

// stripOrphanedToolCalls returns a copy of messages with tool calls that have
// no matching tool result removed (and any result that references a call we
// never emitted). This happens after an abort or provider error mid-tool-call:
// the assistant message keeps a tool_call that was never executed, and
// providers reject such a sequence (e.g. OpenAI "Format Error" 400), poisoning
// every later turn. The original transcript is left untouched — the user still
// sees the aborted message, but the provider never receives the invalid tail.
func stripOrphanedToolCalls(messages []*ai.Message) []*ai.Message {
	emitted := map[string]bool{}
	for _, m := range messages {
		if m.Role == ai.RoleAssistant {
			for _, c := range m.Content {
				if c.Type == ai.ContentTypeToolCall {
					emitted[c.ToolCallID] = true
				}
			}
		}
	}
	answered := map[string]bool{}
	for _, m := range messages {
		if m.Role == ai.RoleToolResult && m.ToolCallID != "" && emitted[m.ToolCallID] {
			answered[m.ToolCallID] = true
		}
	}
	out := make([]*ai.Message, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case ai.RoleAssistant:
			if !hasToolCall(m) {
				out = append(out, m)
				continue
			}
			cp := m.Clone()
			cp.Content = make([]ai.ContentBlock, 0, len(m.Content))
			kept := false
			for _, c := range m.Content {
				if c.Type == ai.ContentTypeToolCall && !answered[c.ToolCallID] {
					continue // orphaned: the tool never produced a result
				}
				cp.Content = append(cp.Content, c)
				if c.Type == ai.ContentTypeText || c.Type == ai.ContentTypeToolCall {
					kept = true
				}
			}
			if kept {
				out = append(out, cp)
			}
			// else: the message was only an orphaned tool call → drop it
		case ai.RoleToolResult:
			if m.ToolCallID == "" || emitted[m.ToolCallID] {
				out = append(out, m)
			}
			// else: result for a call we never emitted → drop it
		default:
			out = append(out, m)
		}
	}
	return out
}

func hasToolCall(m *ai.Message) bool {
	for _, c := range m.Content {
		if c.Type == ai.ContentTypeToolCall {
			return true
		}
	}
	return false
}

func findTool(tools []*AgentTool, name string) *AgentTool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func toolsToLlm(tools []*AgentTool) []*ai.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]*ai.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, &ai.Tool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

func aiMessageError(message string) *ai.Message {
	return &ai.Message{
		Role:         ai.RoleAssistant,
		StopReason:   ai.StopReasonError,
		ErrorMessage: message,
		Content:      []ai.ContentBlock{},
		Timestamp:    time.Now().UnixMilli(),
	}
}

// shortJSON renders a tool argument map for logs, bounded so a huge command
// cannot flood the diagnostic log.
func shortJSON(v any) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(data) > 200 {
		return string(data[:200]) + "…"
	}
	return string(data)
}
