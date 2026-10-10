package ai

import (
	"context"
	"errors"
	"io"
	"time"

	"doge/pkg/logx"
)

// ErrStallTimeout reports a provider stream that produced no data within the
// idle budget. The connection stayed open but the provider went silent, so the
// stream is aborted rather than left to block the agent run forever.
var ErrStallTimeout = errors.New("provider stalled: no data received")

// stallReader wraps an SSE response body so a silent provider (no bytes on an
// otherwise-open connection) aborts the request via cancel and surfaces
// ErrStallTimeout instead of blocking the read forever. A goroutine per Read
// is cheap here: SSE bodies are consumed in large chunks, and only the final
// blocking read ever reaches the timeout path.
type stallReader struct {
	r       io.Reader
	timeout time.Duration
	cancel  context.CancelFunc // aborts the in-flight request on stall
}

// stallCancelWait bounds how long a stall waits for the aborted read to
// return. Cancellation normally unblocks it immediately; the bound is a
// fallback so a misbehaving transport cannot re-hang the stream after we
// already decided to abort.
const stallCancelWait = 5 * time.Second

func (s *stallReader) Read(p []byte) (int, error) {
	type readResult struct {
		n   int
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		n, err := s.r.Read(p)
		done <- readResult{n, err}
	}()
	t := time.NewTimer(s.timeout)
	defer t.Stop()
	select {
	case res := <-done:
		return res.n, res.err
	case <-t.C:
	}
	// Data may have arrived in the same instant the timer fired; drain it
	// before declaring a stall.
	select {
	case res := <-done:
		return res.n, res.err
	default:
	}
	// Cancel the request so the blocked read returns, then report the stall.
	logx.Warnf("[ai] stream stalled: no data for %s, aborting request", s.timeout)
	s.cancel()
	select {
	case <-done:
	case <-time.After(stallCancelWait):
	}
	return 0, ErrStallTimeout
}
