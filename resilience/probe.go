// Package resilience wraps a probe with the client-side policies a real
// caller would have: retries and a circuit breaker.
//
// It sits between the load runner and the probe, so from the runner's point of
// view one Do call is still one logical request — which is exactly the
// accounting that makes retry amplification visible. The caller made one
// request; the target may have received five.
package resilience

import (
	"context"
	"time"

	"github.com/saikiransangam/shakedown/breaker"
	"github.com/saikiransangam/shakedown/load"
	"github.com/saikiransangam/shakedown/metrics"
	"github.com/saikiransangam/shakedown/retry"
)

type Probe struct {
	inner   load.Probe
	policy  retry.Policy
	breaker *breaker.Breaker
}

// Wrap returns a probe applying the given policies. Either may be zero or
// nil, in which case that policy is skipped.
func Wrap(inner load.Probe, policy retry.Policy, br *breaker.Breaker) *Probe {
	return &Probe{inner: inner, policy: policy, breaker: br}
}

func (p *Probe) Do(ctx context.Context) metrics.Result {
	start := time.Now()
	if p.policy.Budget != nil {
		p.policy.Budget.Deposit()
	}

	maxAttempts := max(p.policy.MaxAttempts, 1)
	var result metrics.Result
	attempts := 0

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if p.breaker != nil && !p.breaker.Allow() {
			if attempts == 0 {
				// Shed before anything was sent: the caller gets an instant
				// failure, which is the whole point of the breaker.
				return metrics.Result{
					Start:    start,
					Latency:  time.Since(start),
					Outcome:  metrics.OutcomeCircuitOpen,
					Attempts: 0,
				}
			}
			// Shed partway through retrying: keep the real failure we already
			// measured rather than masking it with our own decision.
			break
		}

		result = p.inner.Do(ctx)
		attempts++

		if p.breaker != nil {
			p.breaker.Record(result.Outcome.ServerFault())
		}

		if !result.Outcome.Retryable() || attempt == maxAttempts {
			break
		}
		// Our own cancellation is not a failure worth retrying into.
		if ctx.Err() != nil {
			break
		}
		if p.policy.Budget != nil && !p.policy.Budget.Withdraw() {
			break
		}
		if !sleep(ctx, p.policy.Delay(attempt)) {
			break
		}
	}

	// Latency is measured from the caller's first attempt through the last,
	// backoff waits included: that is what the caller actually experienced,
	// and it is why retries can make p99 far worse than the underlying
	// service's own latency.
	result.Start = start
	result.Latency = time.Since(start)
	result.Attempts = attempts
	return result
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
