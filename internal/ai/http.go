package ai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"doge/pkg/logx"
)

// Version is the doge version stamped into the default User-Agent. main sets
// it from the build-stamped version (see cmd/doge); the default keeps plain
// `go build` and library users sane.
var Version = "0.3.2"

// defaultUserAgent returns the User-Agent sent with every request unless a
// caller overrides it. Computed per request so a Version set at startup is
// reflected, and overridable per model via a "User-Agent" header in models.json.
func defaultUserAgent() string {
	return "doge/" + Version + " (" + runtime.GOOS + " " + runtime.GOARCH + ")"
}

// maxErrorBody limits how much of an error response body is read.
const maxErrorBody = 64 << 10

// httpClient is the shared streaming HTTP client. Requests carry the caller's
// context so cancellation aborts the request and the body read.
//
// DisableKeepAlives is required for correct cancellation semantics: without
// it, net/http keeps the connection open when a request is canceled by its
// context (waiting for a response that never arrives), so the server never
// observes the disconnect. LLM streaming requests are long-lived SSE
// connections, so the per-request connection cost is irrelevant.
//
// ResponseHeaderTimeout bounds how long a request may wait for response
// headers: a provider that accepts the connection but never responds would
// otherwise hang the stream (and the agent run) in the initial Do call, before
// the mid-stream stall watchdog (stallReader) even starts.
//
// The transport is immutable after creation; SetStreamIdleTimeout replaces the
// whole client so changing the budget never races with in-flight reads of the
// transport fields.
var (
	httpClientMu sync.RWMutex
	httpClient   = &http.Client{Transport: streamingTransport(defaultStreamIdleTimeout)}
)

// streamingTransport builds the shared HTTP transport with the given
// response-header budget.
func streamingTransport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: headerTimeout,
	}
}

// streamClient returns the current shared streaming client.
func streamClient() *http.Client {
	httpClientMu.RLock()
	defer httpClientMu.RUnlock()
	return httpClient
}

// streamIdleTimeout is the provider stall budget: how long an open SSE
// connection may go without bytes before the stream aborts. 0 disables the
// watchdog. Configured process-wide at startup via SetStreamIdleTimeout.
var streamIdleTimeout atomic.Int64

func init() {
	streamIdleTimeout.Store(int64(defaultStreamIdleTimeout))
}

// defaultStreamIdleTimeout is the stall budget when unconfigured. Generous
// enough that slow providers (long thinking pauses, gateways that trickle)
// are not falsely cut off, short enough that a dead provider releases the
// agent run instead of freezing the session.
const defaultStreamIdleTimeout = 2 * time.Minute

// SetStreamIdleTimeout configures the provider stall budget process-wide. The
// shared http client's response-header timeout follows it. Call once at
// startup; 0 disables both the header timeout and the mid-stream watchdog.
func SetStreamIdleTimeout(d time.Duration) {
	streamIdleTimeout.Store(int64(d))
	httpClientMu.Lock()
	httpClient = &http.Client{Transport: streamingTransport(d)}
	httpClientMu.Unlock()
}

// stallTimeout returns the configured stall budget, or 0 when the watchdog is
// disabled (SetStreamIdleTimeout(0)).
func stallTimeout() time.Duration {
	return time.Duration(streamIdleTimeout.Load())
}

// StreamIdleTimeout reports the current provider stall budget (0 = watchdog
// disabled). Tests use it to save and restore the process-wide setting.
func StreamIdleTimeout() time.Duration {
	return stallTimeout()
}

// stallWrapped wraps an SSE body with the stall watchdog, or returns the body
// unchanged when the watchdog is disabled.
func stallWrapped(r io.Reader, cancel context.CancelFunc) io.Reader {
	if idle := stallTimeout(); idle > 0 {
		return &stallReader{r: r, timeout: idle, cancel: cancel}
	}
	return r
}

// stallMessage renders the user-facing message for a stalled provider.
func stallMessage() string {
	return fmt.Sprintf("provider stalled: no data for %s", stallTimeout())
}

// doStreamRequest performs a POST request that expects a streaming (SSE)
// response. headers are merged after the defaults.
func doStreamRequest(ctx context.Context, url string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Defaults are set first, then caller headers override them. An empty
	// caller value suppresses the header, including these defaults: it is set
	// to "" explicitly so the transport does not backfill its own default
	// (e.g. Go-http-client/1.1) — matching Pi, where a null header value
	// removes the header and any default for it.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent())
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return streamClient().Do(req)
}

// retryableStatus reports whether a transient gateway failure is worth
// retrying. Aggregator gateways intermittently return 429 or 5xx for
// routing/rate reasons (e.g. "Router.Unavailable"); a bounded retry
// absorbs those without masking real errors.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// upstreamTransient400 reports whether a 400 response is a transient
// upstream error rather than a client error. Some gateways pass through
// their provider's internal failures as 400 (e.g. SenseTime's
// "InternalError.Algo…"); those are worth a bounded retry even though 400
// normally is not. The body is buffered so the caller can still surface
// the error when retries are exhausted.
func upstreamTransient400(resp *http.Response) bool {
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return bytes.Contains(body, []byte("InternalError.Algo"))
}

// doStreamRequestRetry performs doStreamRequest with bounded retries on
// transient failures (network errors, 429, 5xx, upstream 400s). The
// request body is rebuilt for each attempt via buildBody, and the request
// context deadline bounds the total time spent. A non-retryable status —
// or the final attempt's status, retryable or not — is returned to the
// caller so the adapter can surface the gateway's own error message.
func doStreamRequestRetry(ctx context.Context, url string, headers map[string]string, buildBody func() ([]byte, error)) (*http.Response, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		body, err := buildBody()
		if err != nil {
			return nil, err
		}
		logx.Debugf("[ai] request body %d bytes: %s", len(body), truncateForLog(string(body), 4000))
		started := time.Now()
		resp, err := doStreamRequest(ctx, url, headers, body)
		if err != nil {
			logx.Warnf("[ai] request %s attempt %d FAILED after %s: %v", url, attempt+1, time.Since(started).Round(time.Millisecond), err)
			// A timeout means the provider did not answer within the stall
			// budget (ResponseHeaderTimeout): retrying just multiplies the
			// wait, so fail fast — the caller surfaces the error and the user
			// can re-prompt or abort.
			if os.IsTimeout(err) {
				return nil, err
			}
			lastErr = err
			if attempt == maxAttempts-1 {
				return nil, err
			}
		} else {
			logx.Infof("[ai] request %s -> %d in %s (attempt %d)", url, resp.StatusCode, time.Since(started).Round(time.Millisecond), attempt+1)
			retry := retryableStatus(resp.StatusCode) || upstreamTransient400(resp)
			if !retry || attempt == maxAttempts-1 {
				return resp, nil
			}
			logx.Warnf("[ai] retryable status %d; retrying", resp.StatusCode)
			resp.Body.Close()
		}
		// Exponential backoff bounded by the request deadline. Rate-limit
		// responses back off harder: rpm windows are typically 60s, so give
		// a short burst of attempts (500ms/1s/2s) a chance to pass.
		delay := time.Duration(500*(1<<attempt)) * time.Millisecond
		if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
			delay *= 2
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// formatHTTPError renders a non-2xx response as a readable error string.
func formatHTTPError(status int, body []byte) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}

// readErrorBody reads a bounded error response body.
func readErrorBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return ""
	}
	return string(body)
}

// makeErrorMessage creates the final assistant message encoding a failure.
// Failures are communicated to the agent loop via StopReason, never via Go
// errors, following the contract.
func makeErrorMessage(model *Model, stopReason, message string) *Message {
	return &Message{
		Role:         RoleAssistant,
		API:          model.API,
		Provider:     model.Provider,
		Model:        model.ID,
		StopReason:   stopReason,
		ErrorMessage: message,
		Timestamp:    time.Now().UnixMilli(),
	}
}

// makeAbortedMessage creates the final message for a canceled request.
func makeAbortedMessage(model *Model) *Message {
	return makeErrorMessage(model, StopReasonAborted, "aborted")
}

// truncateForLog bounds a debug-log payload so huge request bodies cannot
// flood the diagnostic log.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
