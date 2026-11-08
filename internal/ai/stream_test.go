package ai

import (
	"errors"
	"testing"
	"time"
)

func TestEventStreamSequence(t *testing.T) {
	s := NewEventStream(4)

	producer := func() {
		partial := NewAssistantMessage("openai-responses", "openai", "gpt-5.2")
		s.Push(AssistantMessageEvent{Type: EventStart, Partial: copyMessage(partial)})
		partial.Content = append(partial.Content, ContentBlock{Type: ContentTypeText, Text: "Hel"})
		s.Push(AssistantMessageEvent{Type: EventTextDelta, Partial: copyMessage(partial), Delta: "Hel"})
		partial.Content[0].Text = "Hello"
		s.Push(AssistantMessageEvent{Type: EventTextDelta, Partial: copyMessage(partial), Delta: "llo"})
		partial.StopReason = StopReasonStop
		s.Push(DoneEvent(partial))
		s.End(partial)
	}

	var events []AssistantMessageEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Range(func(ev AssistantMessageEvent) error {
			events = append(events, ev)
			return nil
		})
	}()
	producer()
	<-done

	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	if events[0].Type != EventStart || events[3].Type != EventDone {
		t.Fatalf("bad event sequence: %v", events)
	}
	if events[1].Delta != "Hel" || events[2].Delta != "llo" {
		t.Errorf("deltas mismatch: %+v", events)
	}
	// Every event carries the accumulated partial snapshot.
	if events[1].Partial.Content[0].Text != "Hel" {
		t.Errorf("partial snapshot not carried: %+v", events[1])
	}
	if events[3].Reason != StopReasonStop {
		t.Errorf("done reason = %q, want %q", events[3].Reason, StopReasonStop)
	}
	result, err := s.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.StopReason != StopReasonStop {
		t.Errorf("result stop reason = %q, want %q", result.StopReason, StopReasonStop)
	}
}

func TestEventStreamErrorEvent(t *testing.T) {
	s := NewEventStream(2)
	final := NewAssistantMessage("openai-responses", "openai", "gpt-5.2")
	final.StopReason = StopReasonError
	final.ErrorMessage = "boom"

	go func() {
		s.Push(ErrorEvent(final))
		s.End(final)
	}()
	result, err := s.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.StopReason != StopReasonError || result.ErrorMessage != "boom" {
		t.Errorf("failure not encoded in message: %+v", result)
	}
}

func TestEventStreamBackpressure(t *testing.T) {
	s := NewEventStream(1)
	partial := NewAssistantMessage("", "", "")

	pushed := make(chan struct{})
	go func() {
		s.Push(AssistantMessageEvent{Type: EventStart, Partial: partial})
		s.Push(AssistantMessageEvent{Type: EventTextDelta, Partial: partial}) // would block if not consumed
		s.Push(DoneEvent(partial))
		s.End(partial)
		close(pushed)
	}()

	// Drain slowly; the producer must not deadlock.
	time.Sleep(10 * time.Millisecond)
	_ = s.Range(func(AssistantMessageEvent) error { return nil })
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("producer deadlocked")
	}
}

func TestEventStreamPushAfterEnd(t *testing.T) {
	s := NewEventStream(1)
	msg := NewAssistantMessage("", "", "")
	s.End(msg)
	// Must not panic or block, even racing with the close.
	for i := 0; i < 100; i++ {
		go s.Push(AssistantMessageEvent{Type: EventTextDelta, Partial: msg})
	}
	s.End(msg) // double End is a no-op
	result, err := s.Result()
	if err != nil || result != msg {
		t.Fatalf("result = %v, %v", result, err)
	}
}

// TestEventStreamConsumerCloseUnblocksProducer：消费侧 Close 必须解除卡在
// Push 上的生产者，且 Result 返回 ErrStreamClosed 哨兵。
func TestEventStreamConsumerCloseUnblocksProducer(t *testing.T) {
	s := NewEventStream(1)
	partial := NewAssistantMessage("", "", "")
	s.Push(AssistantMessageEvent{Type: EventStart, Partial: partial}) // 占满缓冲
	pushed := make(chan struct{})
	go func() {
		s.Push(AssistantMessageEvent{Type: EventTextDelta, Partial: partial}) // 缓冲已满，阻塞
		close(pushed)
	}()
	time.Sleep(20 * time.Millisecond) // 等生产者卡进 Push
	s.Close()
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the producer parked in Push")
	}
	if _, err := s.Result(); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("result err = %v, want ErrStreamClosed", err)
	}
	s.Close() // 幂等：重复 Close 不得 panic
}

// TestEventStreamCloseEndAnyOrder：Close 与 End 任意顺序、任意次数组合都
// 安全，End 仍能记录最终消息。
func TestEventStreamCloseEndAnyOrder(t *testing.T) {
	msg := NewAssistantMessage("", "", "")

	// Close 后 End：End 的最终消息仍由 Result 返回
	s1 := NewEventStream(1)
	s1.Close()
	s1.Close()
	s1.End(msg)
	if got, err := s1.Result(); err != nil || got != msg {
		t.Fatalf("End after Close: result = %v, %v; want msg, nil", got, err)
	}

	// End 后 Close：不得 panic，Result 仍返回消息
	s2 := NewEventStream(1)
	s2.End(msg)
	s2.Close()
	if got, err := s2.Result(); err != nil || got != msg {
		t.Fatalf("Close after End: result = %v, %v; want msg, nil", got, err)
	}

	// 仅 Close：Range 立即返回，不阻塞
	s3 := NewEventStream(1)
	s3.Push(AssistantMessageEvent{Type: EventStart, Partial: msg})
	s3.Close()
	if err := s3.Range(func(AssistantMessageEvent) error { return nil }); err != nil {
		t.Fatalf("range after close: %v", err)
	}
}

func TestLazyStreamSetupError(t *testing.T) {
	s := LazyStream(func() (*EventStream, error) {
		return nil, errSetup
	})
	result, err := s.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.StopReason != StopReasonError || result.ErrorMessage == "" {
		t.Errorf("setup error not encoded: %+v", result)
	}
}

func TestLazyStreamForwards(t *testing.T) {
	inner := NewEventStream(2)
	msg := NewAssistantMessage("openai-responses", "openai", "gpt-5.2")
	msg.StopReason = StopReasonStop
	msg.Content = []ContentBlock{TextBlock("hi")}

	s := LazyStream(func() (*EventStream, error) {
		go func() {
			inner.Push(AssistantMessageEvent{Type: EventStart, Partial: msg})
			inner.Push(DoneEvent(msg))
			inner.End(msg)
		}()
		return inner, nil
	})

	var got []string
	_ = s.Range(func(ev AssistantMessageEvent) error {
		got = append(got, ev.Type)
		return nil
	})
	if len(got) != 2 || got[0] != EventStart || got[1] != EventDone {
		t.Errorf("forwarded events mismatch: %v", got)
	}
	result, err := s.Result()
	if err != nil || result.Text() != "hi" {
		t.Errorf("result = %v, %v", result, err)
	}
}

var errSetup = &setupError{}

type setupError struct{}

func (*setupError) Error() string { return "setup failed" }

// copyMessage returns a deep-enough copy of m for snapshot tests: the struct
// and the content block slice are copied (string fields are immutable).
func copyMessage(m *Message) *Message { return m.Clone() }

// TestRangeNeverSkipsTerminalEvent stresses the Push/End race: the terminal
// event must always be observed even when End fires right after Push.
func TestRangeNeverSkipsTerminalEvent(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := NewEventStream(8)
		go func() {
			for j := 0; j < 20; j++ {
				s.Push(AssistantMessageEvent{Type: EventTextDelta, Delta: "x"})
			}
			s.Push(DoneEvent(NewAssistantMessage("api", "prov", "m")))
			s.End(NewAssistantMessage("api", "prov", "m"))
		}()
		count := 0
		gotDone := false
		err := s.Range(func(ev AssistantMessageEvent) error {
			count++
			if ev.Type == EventDone {
				gotDone = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("run %d: range error: %v", i, err)
		}
		if !gotDone {
			t.Fatalf("run %d: terminal event skipped (count=%d)", i, count)
		}
	}
}
