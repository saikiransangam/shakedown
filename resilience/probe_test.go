package resilience

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saikiransangam/shakedown/breaker"
	"github.com/saikiransangam/shakedown/load"
	"github.com/saikiransangam/shakedown/metrics"
	"github.com/saikiransangam/shakedown/retry"
)

// scriptedProbe returns the given outcomes in order, repeating the last one.
func scriptedProbe(calls *atomic.Int64, outcomes ...metrics.Outcome) load.Probe {
	return load.ProbeFunc(func(ctx context.Context) metrics.Result {
		n := int(calls.Add(1)) - 1
		if n >= len(outcomes) {
			n = len(outcomes) - 1
		}
		return metrics.Result{
			Start:    time.Now(),
			Latency:  time.Millisecond,
			Outcome:  outcomes[n],
			Attempts: 1,
		}
	})
}

func noWait() retry.Policy {
	return retry.Policy{
		MaxAttempts: 3,
		BaseDelay:   time.Nanosecond,
		MaxDelay:    time.Nanosecond,
		Strategy:    retry.StrategyFixed,
		Jitter:      retry.JitterNone,
	}
}

func TestNoRetryWhenFirstAttemptSucceeds(t *testing.T) {
	var calls atomic.Int64
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeSuccess), noWait(), nil)

	res := p.Do(context.Background())

	if calls.Load() != 1 {
		t.Errorf("probe called %d times, want 1", calls.Load())
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if res.Outcome != metrics.OutcomeSuccess {
		t.Errorf("outcome = %q", res.Outcome)
	}
}

func TestRetriesUntilSuccess(t *testing.T) {
	var calls atomic.Int64
	p := Wrap(scriptedProbe(&calls,
		metrics.OutcomeTimeout,
		metrics.OutcomeServerError,
		metrics.OutcomeSuccess,
	), noWait(), nil)

	res := p.Do(context.Background())

	if res.Outcome != metrics.OutcomeSuccess {
		t.Errorf("outcome = %q, want success on the third attempt", res.Outcome)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
}

func TestStopsAtMaxAttempts(t *testing.T) {
	var calls atomic.Int64
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeServerError), noWait(), nil)

	res := p.Do(context.Background())

	if calls.Load() != 3 {
		t.Errorf("probe called %d times, want 3 (MaxAttempts)", calls.Load())
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
	if res.Outcome != metrics.OutcomeServerError {
		t.Errorf("outcome = %q, want the final failure", res.Outcome)
	}
}

// A 4xx will be a 4xx next time too, so retrying it is pure waste.
func TestDoesNotRetryNonRetryableOutcomes(t *testing.T) {
	for _, outcome := range []metrics.Outcome{
		metrics.OutcomeClientError,
		metrics.OutcomeTLSFailure,
		metrics.OutcomeCanceled,
	} {
		var calls atomic.Int64
		p := Wrap(scriptedProbe(&calls, outcome), noWait(), nil)
		p.Do(context.Background())
		if calls.Load() != 1 {
			t.Errorf("%s: probe called %d times, want 1", outcome, calls.Load())
		}
	}
}

func TestLatencyIncludesBackoffWaits(t *testing.T) {
	var calls atomic.Int64
	policy := retry.Policy{
		MaxAttempts: 3,
		BaseDelay:   40 * time.Millisecond,
		MaxDelay:    time.Second,
		Strategy:    retry.StrategyFixed,
		Jitter:      retry.JitterNone,
	}
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeServerError), policy, nil)

	res := p.Do(context.Background())

	// Two backoff waits of 40ms happen between three attempts. The caller
	// waited for those, so they must be in the reported latency.
	if res.Latency < 80*time.Millisecond {
		t.Errorf("latency = %v, want at least 80ms of backoff included", res.Latency)
	}
}

func TestBudgetLimitsRetries(t *testing.T) {
	var calls atomic.Int64
	policy := noWait()
	policy.Budget = retry.NewBudget(0.1)
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeServerError), policy, nil)

	// 100 failing requests at a 10% budget: without it, 3 attempts each
	// would be 300 attempts.
	for range 100 {
		p.Do(context.Background())
	}

	if calls.Load() > 130 {
		t.Errorf("budget ignored: %d attempts for 100 requests", calls.Load())
	}
	if calls.Load() < 100 {
		t.Errorf("only %d attempts for 100 requests; first attempts must never be budgeted", calls.Load())
	}
}

func TestBreakerShedsWithoutSending(t *testing.T) {
	var calls atomic.Int64
	br := breaker.New(breaker.Config{
		FailureThreshold: 0.5,
		MinRequests:      10,
		Window:           time.Second,
		Cooldown:         time.Minute,
		HalfOpenProbes:   1,
	})
	policy := retry.Policy{MaxAttempts: 1}
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeServerError), policy, br)

	for range 10 {
		p.Do(context.Background())
	}
	before := calls.Load()

	res := p.Do(context.Background())

	if res.Outcome != metrics.OutcomeCircuitOpen {
		t.Errorf("outcome = %q, want circuit_open", res.Outcome)
	}
	if res.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0 — a shed request never reaches the target", res.Attempts)
	}
	if calls.Load() != before {
		t.Error("the probe was called despite the breaker being open")
	}
	// Failing fast is the entire point.
	if res.Latency > 5*time.Millisecond {
		t.Errorf("shed request took %v, expected it to be near-instant", res.Latency)
	}
}

func TestBreakerStaysClosedOnClientErrors(t *testing.T) {
	var calls atomic.Int64
	br := breaker.New(breaker.Config{
		FailureThreshold: 0.5,
		MinRequests:      10,
		Window:           time.Second,
		Cooldown:         time.Minute,
	})
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeClientError), retry.Policy{MaxAttempts: 1}, br)

	for range 50 {
		p.Do(context.Background())
	}

	// One caller sending bad requests must not cut everyone else off.
	if br.State() != breaker.StateClosed {
		t.Errorf("state = %v, want closed — 4xx is not the target's fault", br.State())
	}
}

func TestCanceledContextStopsRetrying(t *testing.T) {
	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := Wrap(scriptedProbe(&calls, metrics.OutcomeTimeout), noWait(), nil)
	p.Do(ctx)

	if calls.Load() != 1 {
		t.Errorf("probe called %d times after cancellation, want 1", calls.Load())
	}
}

func TestZeroPolicyBehavesAsNoRetries(t *testing.T) {
	var calls atomic.Int64
	p := Wrap(scriptedProbe(&calls, metrics.OutcomeServerError), retry.Policy{}, nil)

	res := p.Do(context.Background())

	if calls.Load() != 1 {
		t.Errorf("probe called %d times, want 1", calls.Load())
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
}
