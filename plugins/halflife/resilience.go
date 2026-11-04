package halflife

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Failure handling (Task 37). Verification depends on two services reached
// through the gateway: Tavily (via the relay) and the verifier LLM. When one
// is failing, every audit would otherwise wait out the same timeouts and
// retries, and keep spending on calls that fail. So:
//
//   - each failure is classified (failureKind);
//   - a short Retry-After (≤ maxRetryAfter) is waited out and the call
//     retried once; a longer one opens the dependency's circuit until then;
//   - consecutive failures (timeouts, network errors, 5xx, 429) open the
//     circuit for a cooldown, after which one probe call decides whether it
//     closes; while open, calls are skipped and claims say why;
//   - Tavily's 432/433 (credits exhausted) open the Tavily circuit for the
//     credits cooldown: nothing will succeed until the plan changes;
//   - a 402 (the tenant's verifier budget is spent) stops that tenant's
//     audits for the budget backoff, without a call (budget-aware
//     degradation); other tenants are unaffected.

// Failure kinds, as recorded on claims and counted in metrics.
const (
	FailureTimeout          = "timeout"
	FailureNetwork          = "network"
	FailureRateLimited      = "rate_limited"
	FailureServerError      = "server_error"
	FailureOutOfCredits     = "out_of_credits"
	FailureBudget           = "budget"
	FailureLiveFetchRefused = "live_fetch_refused"
	FailureClientError      = "client_error"
	FailureBadReply         = "bad_reply"
	FailureCanceled         = "canceled"
	FailureCircuitOpen      = "circuit_open"
)

// Dependencies, for breakers and metrics.
const (
	DependencyTavily = "tavily"
	DependencyLLM    = "llm"
)

// statusError is a non-2xx reply from the gateway (TavilyError, LLMError).
type statusError interface {
	error
	httpStatus() int
}

func (e *TavilyError) httpStatus() int { return e.Status }
func (e *LLMError) httpStatus() int    { return e.Status }

// failureKind classifies an error from a Tavily or LLM call; "" for nil.
func failureKind(err error) string {
	if err == nil {
		return ""
	}
	var se statusError
	if errors.As(err, &se) {
		switch s := se.httpStatus(); {
		case s == http.StatusTooManyRequests:
			return FailureRateLimited
		case s == http.StatusPaymentRequired:
			return FailureBudget
		case s == 432 || s == 433:
			return FailureOutOfCredits
		case s == 434:
			return FailureLiveFetchRefused
		case s >= 500:
			return FailureServerError
		}
		return FailureClientError
	}
	var ue *UnavailableError
	if errors.As(err, &ue) {
		return FailureCircuitOpen
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	case errors.As(err, &ne):
		return FailureNetwork
	}
	return FailureBadReply
}

// countsAsOutage reports whether a failure says the dependency is unhealthy,
// as opposed to the request being refused for its own reasons.
func countsAsOutage(kind string) bool {
	switch kind {
	case FailureTimeout, FailureNetwork, FailureRateLimited, FailureServerError:
		return true
	}
	return false
}

// UnavailableError is returned instead of calling a dependency whose circuit
// is open.
type UnavailableError struct {
	Dependency string
	Reason     string
	Until      time.Time
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s unavailable (circuit open until %s): %s", e.Dependency, e.Until.UTC().Format(time.RFC3339), e.Reason)
}

// breaker is a circuit breaker for one dependency, shared by every audit.
type breaker struct {
	name      string
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu        sync.Mutex
	failures  int // consecutive outage failures
	openUntil time.Time
	reason    string
	probing   bool // a probe call is in flight after the cooldown
	opens     uint64
}

// BreakerState is a snapshot, for metrics and the startup log.
type BreakerState struct {
	State     string    `json:"state"` // closed, open or half_open
	Failures  int       `json:"consecutive_failures"`
	OpenUntil time.Time `json:"open_until,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Opens     uint64    `json:"opens"`
}

func newBreaker(name string, threshold int, cooldown time.Duration) *breaker {
	return &breaker{name: name, threshold: threshold, cooldown: cooldown, now: time.Now}
}

// allow returns nil when a call may go ahead. After the cooldown it lets one
// probe through; others wait for its result.
func (b *breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return nil
	}
	if b.now().Before(b.openUntil) || b.probing {
		return &UnavailableError{Dependency: b.name, Reason: b.reason, Until: b.openUntil}
	}
	b.probing = true
	return nil
}

// isOpen reports whether calls are being refused right now, without
// claiming the probe.
func (b *breaker) isOpen() (bool, *UnavailableError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() || (!b.now().Before(b.openUntil) && !b.probing) {
		return false, nil
	}
	return true, &UnavailableError{Dependency: b.name, Reason: b.reason, Until: b.openUntil}
}

// record updates the breaker with a call's outcome. Any reply that is not an
// outage (a success, a 402, a 400) shows the dependency is up and closes it.
func (b *breaker) record(err error) {
	kind := failureKind(err)
	b.mu.Lock()
	defer b.mu.Unlock()
	if kind == FailureCanceled || kind == FailureCircuitOpen {
		b.probing = false
		return
	}
	if !countsAsOutage(kind) {
		b.failures, b.openUntil, b.reason, b.probing = 0, time.Time{}, "", false
		return
	}
	b.failures++
	if b.probing || b.failures >= b.threshold {
		b.openLocked(b.now().Add(b.cooldown), fmt.Sprintf("%d consecutive failures, last: %s", b.failures, err))
	}
}

// trip opens the circuit until a given time, whatever its state.
func (b *breaker) trip(until time.Time, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until.After(b.openUntil) {
		b.openLocked(until, reason)
	}
}

func (b *breaker) openLocked(until time.Time, reason string) {
	b.openUntil, b.reason, b.probing = until, clipString(reason, 300), false
	b.opens++
}

func (b *breaker) state() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BreakerState{State: "closed", Failures: b.failures, Opens: b.opens}
	if !b.openUntil.IsZero() {
		s.OpenUntil, s.Reason = b.openUntil, b.reason
		s.State = "open"
		if !b.now().Before(b.openUntil) {
			s.State = "half_open"
		}
	}
	return s
}

// budgetBackoff remembers tenants whose verifier budget refused a call.
type budgetBackoff struct {
	wait time.Duration
	now  func() time.Time

	mu    sync.Mutex
	until map[string]time.Time // tenant -> when audits may try again
}

func newBudgetBackoff(wait time.Duration) *budgetBackoff {
	return &budgetBackoff{wait: wait, now: time.Now, until: make(map[string]time.Time)}
}

func (b *budgetBackoff) spent(tenant string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.until[tenant] = b.now().Add(b.wait)
}

// blocked returns when the tenant's audits may resume, or false.
func (b *budgetBackoff) blocked(tenant string) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.until[tenant]
	if ok && !b.now().Before(t) {
		delete(b.until, tenant)
		return time.Time{}, false
	}
	return t, ok
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d.Round(time.Second)
		}
	}
	return 0
}

// maxRateLimitPause caps how long a long Retry-After keeps a circuit open.
const maxRateLimitPause = 10 * time.Minute

// guarded runs call when b allows it, records the outcome, and opens the
// circuit for a long Retry-After or exhausted Tavily credits.
func guarded[T any](b *breaker, creditsCooldown time.Duration, call func() (T, error)) (T, error) {
	var zero T
	if err := b.allow(); err != nil {
		return zero, err
	}
	out, err := call()
	b.record(err)
	var se statusError
	if !errors.As(err, &se) {
		return out, err
	}
	switch failureKind(err) {
	case FailureOutOfCredits:
		b.trip(b.now().Add(creditsCooldown), "Tavily credits exhausted: "+err.Error())
	case FailureRateLimited:
		if d := retryAfterOf(err); d > maxRetryAfter {
			b.trip(b.now().Add(min(d, maxRateLimitPause)), fmt.Sprintf("rate limited, Retry-After %s", d))
		}
	}
	return out, err
}

// recordLLM records a verifier LLM call's outcome on its circuit, opening it
// for a long Retry-After.
func (v *verifier) recordLLM(err error) {
	v.llmCircuit.record(err)
	if d := retryAfterOf(err); failureKind(err) == FailureRateLimited && d > maxRetryAfter {
		v.llmCircuit.trip(v.llmCircuit.now().Add(min(d, maxRateLimitPause)), fmt.Sprintf("rate limited, Retry-After %s", d))
	}
}

func retryAfterOf(err error) time.Duration {
	var te *TavilyError
	if errors.As(err, &te) {
		return te.RetryAfter
	}
	var le *LLMError
	if errors.As(err, &le) {
		return le.RetryAfter
	}
	return 0
}
