package retry

import (
	"testing"
	"time"
)

func TestDelayExponentialWithoutJitter(t *testing.T) {
	p := Policy{
		MaxAttempts: 10,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Strategy:    StrategyExponential,
		Jitter:      JitterNone,
	}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, expected := range want {
		if got := p.Delay(i + 1); got != expected {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, expected)
		}
	}
}

func TestDelayFixed(t *testing.T) {
	p := Policy{MaxAttempts: 5, BaseDelay: 70 * time.Millisecond, Strategy: StrategyFixed, Jitter: JitterNone}
	for attempt := 1; attempt <= 4; attempt++ {
		if got := p.Delay(attempt); got != 70*time.Millisecond {
			t.Errorf("Delay(%d) = %v, want 70ms", attempt, got)
		}
	}
}

func TestDelayRespectsMaxDelay(t *testing.T) {
	p := Policy{
		MaxAttempts: 40,
		BaseDelay:   time.Second,
		MaxDelay:    5 * time.Second,
		Strategy:    StrategyExponential,
		Jitter:      JitterNone,
	}
	// Attempt 30 would be 2^29 seconds; it must clamp, not overflow.
	if got := p.Delay(30); got != 5*time.Second {
		t.Errorf("Delay(30) = %v, want the 5s cap", got)
	}
}

func TestFullJitterStaysInRange(t *testing.T) {
	p := Policy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		Strategy:    StrategyExponential,
		Jitter:      JitterFull,
	}

	var sawBelowHalf bool
	for range 200 {
		got := p.Delay(3) // unjittered this would be exactly 400ms
		if got < 0 || got >= 400*time.Millisecond {
			t.Fatalf("full jitter produced %v, want [0, 400ms)", got)
		}
		if got < 200*time.Millisecond {
			sawBelowHalf = true
		}
	}
	// Full jitter must be able to pick small delays; that spread is the
	// entire reason it exists.
	if !sawBelowHalf {
		t.Error("full jitter never produced a delay below half the base")
	}
}

func TestEqualJitterKeepsAMinimum(t *testing.T) {
	p := Policy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    time.Second,
		Strategy:    StrategyExponential,
		Jitter:      JitterEqual,
	}
	for range 200 {
		got := p.Delay(3) // unjittered 400ms, so expect [200ms, 400ms]
		if got < 200*time.Millisecond || got > 400*time.Millisecond {
			t.Fatalf("equal jitter produced %v, want [200ms, 400ms]", got)
		}
	}
}

func TestEnabled(t *testing.T) {
	if (Policy{MaxAttempts: 1}).Enabled() {
		t.Error("MaxAttempts 1 means no retries")
	}
	if !(Policy{MaxAttempts: 2}).Enabled() {
		t.Error("MaxAttempts 2 means retries are on")
	}
}

func TestBudgetAllowsRetriesInProportion(t *testing.T) {
	b := NewBudget(0.1) // 10% of requests may be retried

	// 100 requests fund 10 retries.
	for range 100 {
		b.Deposit()
	}
	allowed := 0
	for range 50 {
		if b.Withdraw() {
			allowed++
		}
	}
	if allowed != 10 {
		t.Errorf("allowed %d retries from 100 requests at ratio 0.1, want 10", allowed)
	}
}

func TestBudgetCapsSavedUpTokens(t *testing.T) {
	b := NewBudget(1.0)
	// A long healthy period must not bank an unlimited number of retries.
	for range 1000 {
		b.Deposit()
	}
	allowed := 0
	for range 1000 {
		if b.Withdraw() {
			allowed++
		}
	}
	if allowed != burstAllowance {
		t.Errorf("allowed %d retries, want the %d burst cap", allowed, burstAllowance)
	}
}

func TestBudgetStarvesDuringTotalOutage(t *testing.T) {
	b := NewBudget(0.1)

	// Every request fails and wants to retry: the budget must refuse almost
	// all of them, which is what stops a retry storm.
	retried := 0
	for range 100 {
		b.Deposit()
		if b.Withdraw() {
			retried++
		}
	}
	if retried > 15 {
		t.Errorf("budget allowed %d/100 retries during a total outage, want ~10", retried)
	}
	if retried == 0 {
		t.Error("budget allowed no retries at all")
	}
}

func TestParseStrategyAndJitter(t *testing.T) {
	if _, err := ParseStrategy("Exponential"); err != nil {
		t.Errorf("case-insensitive parse failed: %v", err)
	}
	if _, err := ParseStrategy("linear"); err == nil {
		t.Error("expected an error for an unknown strategy")
	}
	if _, err := ParseJitter("FULL"); err != nil {
		t.Errorf("case-insensitive parse failed: %v", err)
	}
	if _, err := ParseJitter("decorrelated"); err == nil {
		t.Error("expected an error for an unknown jitter mode")
	}
}
