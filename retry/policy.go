// Package retry implements retry policies and the budget that keeps them from
// turning a struggling service into a dead one.
//
// Retries are the most common well-intentioned cause of outages. A service
// slows down, every client retries, the service now receives several times the
// traffic, it slows down further, and the clients retry harder. The load only
// stops rising when something gives up — which is why the interesting part of
// this package is not the backoff maths but [Budget].
package retry

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

type Strategy string

const (
	// StrategyFixed waits the same delay between every attempt.
	StrategyFixed Strategy = "fixed"
	// StrategyExponential doubles the delay each attempt, which gives a
	// struggling target geometrically more room to recover.
	StrategyExponential Strategy = "exponential"
)

type Jitter string

const (
	// JitterNone retries on an exact schedule. Every client that failed at
	// the same moment then retries at the same moment, so the target sees
	// synchronised waves of load rather than a smooth stream.
	JitterNone Jitter = "none"
	// JitterFull picks uniformly from [0, delay). This spreads a thundering
	// herd out completely and is the default worth reaching for.
	JitterFull Jitter = "full"
	// JitterEqual picks from [delay/2, delay), keeping a guaranteed minimum
	// wait while still breaking up synchronisation.
	JitterEqual Jitter = "equal"
)

type Policy struct {
	// MaxAttempts counts the first try. 1 disables retrying.
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Strategy    Strategy
	Jitter      Jitter
	// Budget, when set, caps retries as a share of total requests.
	Budget *Budget
}

const (
	defaultBaseDelay = 50 * time.Millisecond
	defaultMaxDelay  = 2 * time.Second
)

func (p Policy) withDefaults() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaultBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaultMaxDelay
	}
	if p.Strategy == "" {
		p.Strategy = StrategyExponential
	}
	if p.Jitter == "" {
		p.Jitter = JitterFull
	}
	return p
}

// Enabled reports whether this policy will ever retry.
func (p Policy) Enabled() bool { return p.MaxAttempts > 1 }

// Delay returns how long to wait before the given attempt number, where
// attempt 1 is the delay after the first failure.
func (p Policy) Delay(attempt int) time.Duration {
	p = p.withDefaults()
	if attempt < 1 {
		attempt = 1
	}

	delay := p.BaseDelay
	if p.Strategy == StrategyExponential {
		// math.Pow on a float avoids the overflow that 1<<attempt reaches
		// after about 63 attempts.
		scaled := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
		if scaled > float64(p.MaxDelay) {
			delay = p.MaxDelay
		} else {
			delay = time.Duration(scaled)
		}
	}
	if delay > p.MaxDelay {
		delay = p.MaxDelay
	}

	switch p.Jitter {
	case JitterFull:
		if delay > 0 {
			delay = time.Duration(rand.Int64N(int64(delay)))
		}
	case JitterEqual:
		if delay > 0 {
			half := delay / 2
			delay = half + time.Duration(rand.Int64N(int64(half)+1))
		}
	}
	return delay
}

func ParseStrategy(s string) (Strategy, error) {
	switch Strategy(strings.ToLower(s)) {
	case StrategyFixed:
		return StrategyFixed, nil
	case StrategyExponential:
		return StrategyExponential, nil
	default:
		return "", fmt.Errorf("unknown backoff strategy %q (want fixed or exponential)", s)
	}
}

func ParseJitter(s string) (Jitter, error) {
	switch Jitter(strings.ToLower(s)) {
	case JitterNone:
		return JitterNone, nil
	case JitterFull:
		return JitterFull, nil
	case JitterEqual:
		return JitterEqual, nil
	default:
		return "", fmt.Errorf("unknown jitter mode %q (want none, full or equal)", s)
	}
}
