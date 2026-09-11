package grpc

import (
	"fmt"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Rate-limit / budget backoff policy for the backend's abuse-resistance
// enforcement (docs/CLI_CHANGES_REQUIRED.md §2). These delays are LONG by
// design — the backend told the client it stopped processing cheaply, so
// retrying at the normal 2s cadence would defeat the budget's purpose.
//
// They are vars (not consts) so tests can shorten them via the setter below
// instead of waiting out the real production values.
var (
	// authBudgetBackoff: ResourceExhausted "too many failed authentication
	// attempts" (E2). The message carries a server-announced window
	// ("retry after 5m0s"); we back off for minutes, not seconds.
	authBudgetBackoff = 5 * time.Minute
	// streamRateBackoff: stream terminated with ResourceExhausted
	// "message rate exceeded" (E1). At least 30s per the contract so the
	// reconnect snapshot burst cannot compound the budget overrun.
	streamRateBackoff = 30 * time.Second
	// streamRateBackoffCap: ceiling for the escalated stream-rate backoff —
	// a stream that keeps tripping the message budget must not keep
	// re-sending its full snapshot burst at the floor delay.
	streamRateBackoffCap = 5 * time.Minute
	// streamCapBackoff: stream open failed with ResourceExhausted
	// "too many concurrent agent streams" (E3). Capped account — retrying
	// sooner cannot help until another of the account's streams closes.
	streamCapBackoff = 60 * time.Second
)

// streamRateEscalation scales the stream-rate backoff across consecutive
// budget terminations (30s, 60s, 90s, … capped): repeated overruns earn a
// progressively longer pause instead of a burst every 30s. A clean stream
// resets the counter.
func streamRateEscalation(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	d := streamRateBackoff * time.Duration(consecutive)
	if d > streamRateBackoffCap {
		d = streamRateBackoffCap
	}
	return d
}

// setRateLimitBackoffsForTest shortens the budget backoff delays for the
// duration of one test. The production values are never modified by tests
// that use this seam.
func setRateLimitBackoffsForTest(t interface {
	Cleanup(func())
}, authBudget, streamRate, streamCap time.Duration) {
	origAuth, origRate, origCap := authBudgetBackoff, streamRateBackoff, streamCapBackoff
	authBudgetBackoff, streamRateBackoff, streamCapBackoff = authBudget, streamRate, streamCap
	t.Cleanup(func() {
		authBudgetBackoff, streamRateBackoff, streamCapBackoff = origAuth, origRate, origCap
	})
}

// parseRetryAfter parses the server-announced "retry after <duration>"
// suffix the backend appends to failed-auth budget messages (e.g. "too many
// failed authentication attempts; retry after 5m0s"). Returns ok=false when
// no duration is present. The parsed value is trusted only as a floor
// replacement for authBudgetBackoff, never to shorten it.
func parseRetryAfter(msg string) (time.Duration, bool) {
	idx := strings.LastIndex(strings.ToLower(msg), "retry after")
	if idx < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(msg[idx+len("retry after"):])
	// Tolerate a trailing period or further text: take the first token.
	if i := strings.IndexAny(rest, ".;,\n"); i >= 0 {
		rest = rest[:i]
	}
	d, err := time.ParseDuration(rest)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// rateLimitedAuthError builds the structured error returned when an auth
// attempt hits the backend's failed-auth budget (E2). The message tells the
// operator how long the client will back off.
func rateLimitedAuthError(err error) error {
	backoff := authBudgetBackoff
	hint := ""
	if msg, ok := grpcStatusMessage(err); ok {
		if d, ok := parseRetryAfter(msg); ok {
			if d > backoff {
				backoff = d
			}
			hint = fmt.Sprintf(" (server announced retry after %s)", d)
		}
	}
	return phelixerr.Wrapf(
		phelixerr.CodeRateLimited,
		err,
		"backend throttled authentication attempts%s; backing off for %s before retrying",
		hint,
		backoff,
	)
}

// classifyStreamError maps a stream-level gRPC failure to the loop policy
// the monitor/agent stream loops should follow. Normal connection errors
// (including the E5 idle-timeout DeadlineExceeded and io.EOF) keep the
// normal fast reconnect; the backend's ResourceExhausted budgets demand a
// long pause instead.
type streamErrorPolicy struct {
	// Delay to wait before the next stream attempt.
	delay time.Duration
	// Logged message describing the budget that was hit ("" for normal
	// connection errors — the caller logs the raw error).
	reason string
}

func classifyStreamError(err error) streamErrorPolicy {
	switch resourceExhaustedKind(err) {
	case "auth_budget":
		// Honor the server-announced window when it exceeds the default
		// (E2: "retry after 5m0s"); never shorten the default with it.
		delay := authBudgetBackoff
		if msg, ok := grpcStatusMessage(err); ok {
			if d, ok := parseRetryAfter(msg); ok && d > delay {
				delay = d
			}
		}
		return streamErrorPolicy{delay: delay, reason: "backend auth-attempt budget exhausted"}
	case "stream_rate":
		return streamErrorPolicy{delay: streamRateBackoff, reason: "backend stream message-rate budget exceeded"}
	case "stream_cap":
		return streamErrorPolicy{delay: streamCapBackoff, reason: "account concurrent-stream cap reached"}
	case "send_limit":
		// E4: a single event exceeded the 4 MiB receive limit. This is a
		// client bug, not a transient failure — no special delay, the caller
		// logs it loudly (with the message type) and continues.
		return streamErrorPolicy{reason: "event exceeded backend 4 MiB message limit"}
	}
	if isStreamIdleTimeout(err) {
		// E5: reaped for idleness — exactly a disconnect, normal backoff.
		return streamErrorPolicy{}
	}
	return streamErrorPolicy{}
}

// messageTooLargeError reports whether err is the backend's send-time
// MaxRecvMsgSize rejection (E4): gRPC ResourceExhausted whose message is not
// one of the known budget shapes. status.FromError traverses wrapped chains.
func messageTooLargeError(err error) bool {
	return resourceExhaustedKind(err) == "send_limit"
}

// authBudgetError reports whether err is the failed-auth budget rejection
// (E2): ResourceExhausted "too many failed authentication attempts".
func authBudgetError(err error) bool {
	return resourceExhaustedKind(err) == "auth_budget"
}

// streamCapError reports whether err is the account concurrent-stream cap
// rejection (E3).
func streamCapError(err error) bool {
	return resourceExhaustedKind(err) == "stream_cap"
}
