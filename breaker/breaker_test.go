package breaker

import (
	"sync"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		FailureThreshold: 0.5,
		MinRequests:      10,
		Window:           time.Second,
		Cooldown:         100 * time.Millisecond,
		HalfOpenProbes:   2,
	}
}

func TestStartsClosedAndStaysClosedWhileHealthy(t *testing.T) {
	b := New(testConfig())
	for range 100 {
		if !b.Allow() {
			t.Fatal("a healthy breaker must not shed")
		}
		b.Record(false)
	}
	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed", b.State())
	}
}

func TestDoesNotTripBelowMinRequests(t *testing.T) {
	b := New(testConfig())
	// 100% failure rate, but only 5 samples: too little to act on.
	for range 5 {
		b.Allow()
		b.Record(true)
	}
	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed — 5 samples is not evidence", b.State())
	}
}

func TestOpensOnSustainedFailure(t *testing.T) {
	b := New(testConfig())
	for range 10 {
		b.Allow()
		b.Record(true)
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}

	// While open, requests are shed without being sent.
	if b.Allow() {
		t.Error("an open breaker must not allow requests during cooldown")
	}
	if stats := b.Stats(); stats.Shed == 0 {
		t.Error("shed count was not recorded")
	}
}

func TestHalfOpenClosesAfterSuccessfulProbes(t *testing.T) {
	cfg := testConfig()
	b := New(cfg)
	for range 10 {
		b.Allow()
		b.Record(true)
	}

	time.Sleep(cfg.Cooldown + 20*time.Millisecond)

	// First Allow after cooldown moves to half-open and admits a probe.
	if !b.Allow() {
		t.Fatal("expected a probe to be admitted after cooldown")
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %v, want half-open", b.State())
	}
	b.Record(false)

	if !b.Allow() {
		t.Fatal("expected the second probe to be admitted")
	}
	b.Record(false)

	if b.State() != StateClosed {
		t.Errorf("state = %v, want closed after %d good probes", b.State(), cfg.HalfOpenProbes)
	}
}

func TestHalfOpenReopensOnASingleFailedProbe(t *testing.T) {
	cfg := testConfig()
	b := New(cfg)
	for range 10 {
		b.Allow()
		b.Record(true)
	}
	time.Sleep(cfg.Cooldown + 20*time.Millisecond)

	if !b.Allow() {
		t.Fatal("expected a probe after cooldown")
	}
	b.Record(true) // still sick

	if b.State() != StateOpen {
		t.Errorf("state = %v, want open again", b.State())
	}
	if opened := b.Stats().TimesOpened; opened != 2 {
		t.Errorf("TimesOpened = %d, want 2", opened)
	}
}

func TestHalfOpenLimitsProbes(t *testing.T) {
	cfg := testConfig()
	b := New(cfg)
	for range 10 {
		b.Allow()
		b.Record(true)
	}
	time.Sleep(cfg.Cooldown + 20*time.Millisecond)

	admitted := 0
	for range 20 {
		if b.Allow() {
			admitted++
		}
	}
	// A half-open breaker must not itself become a load spike.
	if admitted != cfg.HalfOpenProbes {
		t.Errorf("admitted %d probes, want %d", admitted, cfg.HalfOpenProbes)
	}
}

func TestFailuresAgeOutOfTheWindow(t *testing.T) {
	cfg := testConfig()
	cfg.Window = 200 * time.Millisecond
	cfg.MinRequests = 10
	b := New(cfg)

	// 5 failures: not enough to trip on its own.
	for range 5 {
		b.Allow()
		b.Record(true)
	}
	// Wait for the whole window to roll past them.
	time.Sleep(cfg.Window + 50*time.Millisecond)

	// 5 more failures. If the old ones had persisted, this would reach the
	// 10-request minimum at a 100% failure rate and trip.
	for range 5 {
		b.Allow()
		b.Record(true)
	}
	if b.State() != StateOpen {
		return // aged out correctly
	}
	t.Error("stale failures outside the window still tripped the breaker")
}

func TestStatsReportTimeOpen(t *testing.T) {
	cfg := testConfig()
	b := New(cfg)
	for range 10 {
		b.Allow()
		b.Record(true)
	}
	time.Sleep(50 * time.Millisecond)

	stats := b.Stats()
	if stats.State != StateOpen {
		t.Fatalf("state = %v, want open", stats.State)
	}
	if stats.TimeOpen < 40*time.Millisecond {
		t.Errorf("TimeOpen = %v, want at least ~50ms", stats.TimeOpen)
	}
	if len(stats.Transitions) == 0 {
		t.Error("expected a recorded transition")
	}
}

func TestConcurrentUse(t *testing.T) {
	b := New(testConfig())
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if b.Allow() {
					b.Record(true)
				}
				b.Stats()
			}
		}()
	}
	wg.Wait()
	// Correctness here is "the race detector stays quiet"; the state is
	// nondeterministic by design.
	if b.Stats().TimesOpened == 0 {
		t.Error("expected the breaker to open under an all-failure workload")
	}
}

func TestStateString(t *testing.T) {
	for state, want := range map[State]string{
		StateClosed:   "closed",
		StateOpen:     "open",
		StateHalfOpen: "half-open",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}
