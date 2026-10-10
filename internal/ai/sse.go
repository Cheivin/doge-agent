package ai

import (
	"bufio"
	"bytes"
	"io"
)

// Pre-allocated field prefixes so the per-line parse never allocates.
var (
	sseEventPrefix = []byte("event:")
	sseDataPrefix  = []byte("data:")
)

// SSEEvent is a single parsed Server-Sent Event: an optional event name and
// the accumulated data lines joined with newlines.
type SSEEvent struct {
	Event string
	Data  []byte
}

// readSSELine returns the next line including its trailing newline. The
// returned slice is only valid until the next call, so callers must copy
// anything they retain. Lines longer than the reader's buffer (rare in LLM
// SSE streams) are accumulated across ErrBufferFull fragments.
func readSSELine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return line, err
	}
	buf := make([]byte, 0, len(line)*2)
	buf = append(buf, line...)
	for err == bufio.ErrBufferFull {
		line, err = br.ReadSlice('\n')
		buf = append(buf, line...)
	}
	return buf, err
}

// IterateSSE parses a Server-Sent Events stream, invoking fn for each
// dispatched event. Comments (lines starting with ":") and pings (empty event
// names) are skipped. It returns the first error from fn, or an io error.
func IterateSSE(r io.Reader, fn func(ev SSEEvent) error) error {
	br := bufio.NewReader(r)
	var eventName string
	var data []byte

	dispatch := func() error {
		if len(data) == 0 && eventName == "" {
			return nil
		}
		ev := SSEEvent{Event: eventName, Data: data}
		eventName = ""
		data = nil
		return fn(ev)
	}

	for {
		line, err := readSSELine(br)
		if len(line) > 0 {
			// Strip the trailing newline (and CR for CRLF streams).
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if err := dispatch(); err != nil {
					return err
				}
			case line[0] == ':':
				// Comment; ignore.
			case bytes.HasPrefix(line, sseEventPrefix):
				eventName = string(bytes.TrimSpace(line[len(sseEventPrefix):]))
			case bytes.HasPrefix(line, sseDataPrefix):
				value := line[len(sseDataPrefix):]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
			}
			// Unknown fields are ignored per the SSE spec.
		}
		if err != nil {
			if err == io.EOF {
				// Dispatch any event without a trailing blank line.
				return dispatch()
			}
			return err
		}
	}
}
