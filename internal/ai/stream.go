package ai

import (
	"errors"
	"strings"
	"sync"
)

// Streaming event types. A stream starts with "start" and terminates with
// exactly one of "done" or "error".
const (
	EventStart         = "start"
	EventTextStart     = "text_start"
	EventTextDelta     = "text_delta"
	EventTextEnd       = "text_end"
	EventThinkingStart = "thinking_start"
	EventThinkingDelta = "thinking_delta"
	EventThinkingEnd   = "thinking_end"
	EventToolCallStart = "toolcall_start"
	EventToolCallDelta = "toolcall_delta"
	EventToolCallEnd   = "toolcall_end"
	EventDone          = "done"
	EventError         = "error"
)

// AssistantMessageEvent is a single event from a streaming LLM response.
//
// Every non-terminal event carries Partial, the accumulated assistant message
// snapshot at that point in the stream, so consumers can render without
// aggregating deltas themselves. The terminal event is either done (success)
// or error (failure); both carry the final message via Message/Error, whose
// StopReason is "stop"/"length"/"toolUse" or "error"/"aborted" respectively.
type AssistantMessageEvent struct {
	Type         string
	Partial      *Message
	ContentIndex int
	Delta        string
	ToolCall     *ContentBlock
	Reason       string
	Message      *Message
	Error        *Message
}

// DoneEvent returns the terminal success event for a final message.
func DoneEvent(msg *Message) AssistantMessageEvent {
	return AssistantMessageEvent{
		Type:    EventDone,
		Reason:  msg.StopReason,
		Message: msg,
		Partial: msg,
	}
}

// ErrorEvent returns the terminal error event for a final message whose
// StopReason is "error" or "aborted".
func ErrorEvent(msg *Message) AssistantMessageEvent {
	return AssistantMessageEvent{
		Type:    EventError,
		Reason:  msg.StopReason,
		Error:   msg,
		Partial: msg,
	}
}

// EventStream is a single-consumer stream of assistant message events with a
// terminal result.
//
// Contract: producers push events in order, then call End exactly once with
// the final message. Failures must be encoded as events (an error event with
// a final message whose StopReason is "error" or "aborted") rather than Go
// errors.
//
// Concurrency: the event channel is never closed; consumers terminate when
// they observe the terminal event (via Range, which also unblocks when the
// stream ends), so a send can never race with a close. Push applies
// backpressure when the consumer is behind; events pushed after End are
// dropped.
type EventStream struct {
	ch       chan AssistantMessageEvent
	terminal chan *Message
	done     chan struct{}

	termOnce sync.Once
}

// NewEventStream creates a stream with the given event buffer size.
func NewEventStream(bufferSize int) *EventStream {
	if bufferSize <= 0 {
		bufferSize = 16
	}
	return &EventStream{
		ch:       make(chan AssistantMessageEvent, bufferSize),
		terminal: make(chan *Message, 1),
		done:     make(chan struct{}),
	}
}

// Push delivers an event to the consumer, blocking when the consumer is
// behind. Events pushed after termination are dropped.
func (s *EventStream) Push(ev AssistantMessageEvent) {
	select {
	case s.ch <- ev:
	case <-s.done:
	}
}

// End terminates the stream with the final message. It is safe to call once;
// subsequent calls are no-ops.
func (s *EventStream) End(msg *Message) {
	s.termOnce.Do(func() {
		s.terminal <- msg
		close(s.done)
		close(s.terminal)
	})
}

// Events returns the channel of streamed events. The channel is never
// closed; consumers should use Range or terminate on the terminal event.
func (s *EventStream) Events() <-chan AssistantMessageEvent {
	return s.ch
}

// Range invokes fn for each event until a terminal event is observed or the
// stream ends. Returning an error from fn stops iteration and is returned to
// the caller. Range never returns an error for normal stream termination.
// Buffered events are always drained before the done signal is honored, so
// no event is skipped when the producer finishes ahead of the consumer.
func (s *EventStream) Range(fn func(ev AssistantMessageEvent) error) error {
	process := func(ev AssistantMessageEvent) error {
		if err := fn(ev); err != nil {
			return err
		}
		if ev.Type == EventDone || ev.Type == EventError {
			return errStopRange
		}
		return nil
	}
	for {
		// Drain buffered events first so the terminal event pushed right
		// before End is never skipped.
		select {
		case ev := <-s.ch:
			if err := process(ev); err != nil {
				if err == errStopRange {
					return nil
				}
				return err
			}
			continue
		default:
		}
		// No buffered events: wait for the next event or termination.
		select {
		case ev := <-s.ch:
			if err := process(ev); err != nil {
				if err == errStopRange {
					return nil
				}
				return err
			}
		case <-s.done:
			// End fired; consume any event that was pushed just before it
			// (Push and End race here), then finish.
			select {
			case ev := <-s.ch:
				if err := process(ev); err != nil {
					if err == errStopRange {
						return nil
					}
					return err
				}
			default:
				return nil
			}
		}
	}
}

var errStopRange = errors.New("stop range iteration")

// Result blocks until the stream terminates and returns the final message.
// Failures are encoded in the message's StopReason ("error" or "aborted");
// a Go error is only returned when the stream terminated without a terminal
// message, which indicates misuse of the stream API.
func (s *EventStream) Result() (*Message, error) {
	msg, ok := <-s.terminal
	if !ok {
		return nil, errors.New("stream ended without a terminal message")
	}
	return msg, nil
}

// LazyStream returns a stream that runs setup asynchronously and forwards
// events from the inner stream. Setup failures are encoded as error events,
// preserving the contract that stream functions never throw. Use it for
// async work such as auth resolution before the request starts.
func LazyStream(setup func() (*EventStream, error)) *EventStream {
	s := NewEventStream(16)
	go func() {
		inner, err := setup()
		if err != nil {
			endStream(s, &Message{Role: RoleAssistant, StopReason: StopReasonError, ErrorMessage: err.Error()})
			return
		}
		if err := inner.Range(func(ev AssistantMessageEvent) error {
			s.Push(ev)
			return nil
		}); err != nil {
			endStream(s, &Message{Role: RoleAssistant, StopReason: StopReasonError, ErrorMessage: err.Error()})
			return
		}
		msg, err := inner.Result()
		if err != nil {
			endStream(s, &Message{Role: RoleAssistant, StopReason: StopReasonError, ErrorMessage: err.Error()})
			return
		}
		endStream(s, msg)
	}()
	return s
}

// endStream pushes the terminal event for msg (done or error based on its
// stop reason) and terminates the stream.
func endStream(s *EventStream, msg *Message) {
	if msg.StopReason == StopReasonError || msg.StopReason == StopReasonAborted {
		s.Push(ErrorEvent(msg))
	} else {
		s.Push(DoneEvent(msg))
	}
	s.End(msg)
}

// textAccum accumulates streamed text deltas into a string. Each Append is
// O(1) amortized (strings.Builder), and Current returns the accumulated text
// without copying, so per-delta snapshots never re-copy the whole block (the
// naive "s += d" pattern is O(n²) in the total length and dominates long
// streaming replies and reasoning output).
//
// A string returned by Current aliases the builder's buffer and stays valid
// as long as the builder is only appended to; Replace discards it. This is
// the documented safe pattern for strings.Builder.
type textAccum struct {
	b strings.Builder
}

// Current returns the accumulated text without copying.
func (a *textAccum) Current() string { return a.b.String() }

// Append writes s and returns the accumulated text.
func (a *textAccum) Append(s string) string {
	a.b.WriteString(s)
	return a.b.String()
}

// Replace discards the accumulated text and returns s as the new value. Used
// when a final value supersedes the streamed deltas.
func (a *textAccum) Replace(s string) string {
	a.b.Reset()
	a.b.WriteString(s)
	return a.b.String()
}

// blockAccum returns the text accumulator for the content block at idx,
// lazily creating one seeded with the block's current text so direct writes
// to the block and builder writes stay in sync.
func blockAccum(accums map[int]*textAccum, blocks []ContentBlock, idx int) *textAccum {
	if a := accums[idx]; a != nil {
		return a
	}
	a := &textAccum{}
	if idx >= 0 && idx < len(blocks) {
		switch blocks[idx].Type {
		case ContentTypeText:
			a.Replace(blocks[idx].Text)
		case ContentTypeThinking:
			a.Replace(blocks[idx].Thinking)
		}
	}
	accums[idx] = a
	return a
}
