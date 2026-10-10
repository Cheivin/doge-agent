package ai

import (
	"strings"
	"testing"
)

func TestIterateSSE(t *testing.T) {
	raw := "event: message\n" +
		"data: {\"hello\": \"world\"}\n" +
		"\n" +
		": a comment\n" +
		"data: line1\ndata: line2\n" +
		"\n" +
		"data: no event name\n" +
		"\n" +
		"data: trailing without newline"

	var got []SSEEvent
	if err := IterateSSE(strings.NewReader(raw), func(ev SSEEvent) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	if got[0].Event != "message" || string(got[0].Data) != `{"hello": "world"}` {
		t.Errorf("event 0 mismatch: %+v", got[0])
	}
	if got[1].Event != "" || string(got[1].Data) != "line1\nline2" {
		t.Errorf("event 1 mismatch: %+v", got[1])
	}
	if got[2].Event != "" || string(got[2].Data) != "no event name" {
		t.Errorf("event 2 mismatch: %+v", got[2])
	}
	// Data without a trailing newline is dispatched at EOF.
	if got[3].Event != "" || string(got[3].Data) != "trailing without newline" {
		t.Errorf("event 3 mismatch: %+v", got[3])
	}
}

func TestIterateSSECRLF(t *testing.T) {
	raw := "event: message\r\n" +
		"data: hello\r\n" +
		"\r\n" +
		"data: world\r\n" +
		"\r\n"
	var got []SSEEvent
	if err := IterateSSE(strings.NewReader(raw), func(ev SSEEvent) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(got) != 2 || string(got[0].Data) != "hello" || string(got[1].Data) != "world" {
		t.Errorf("CRLF parsing mismatch: %+v", got)
	}
}

func TestIterateSSEStopsOnError(t *testing.T) {
	raw := "data: a\n\ndata: b\n\n"
	n := 0
	err := IterateSSE(strings.NewReader(raw), func(ev SSEEvent) error {
		n++
		if n == 1 {
			return errStop
		}
		return nil
	})
	if err != errStop || n != 1 {
		t.Errorf("expected early stop, got n=%d err=%v", n, err)
	}
}

var errStop = &stopError{}

type stopError struct{}

func (*stopError) Error() string { return "stop" }
