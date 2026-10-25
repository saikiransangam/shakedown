package metrics

import (
	"testing"
	"time"
)

// collect feeds results through a Collector the way a run does.
func collect(start time.Time, results ...Result) *Summary {
	c := NewCollector(start)
	ch := make(chan Result, len(results))
	for _, r := range results {
		ch <- r
	}
	close(ch)
	return c.Collect(ch)
}

func result(start time.Time, offset, latency time.Duration, outcome Outcome, status int) Result {
	return Result{
		Start:    start.Add(offset),
		Latency:  latency,
		Status:   status,
		Outcome:  outcome,
		Attempts: 1,
	}
}

func TestCollectorCountsOutcomesAndStatuses(t *testing.T) {
	start := time.Now()
	s := collect(start,
		result(start, 0, 10*time.Millisecond, OutcomeSuccess, 200),
		result(start, 0, 12*time.Millisecond, OutcomeSuccess, 200),
		result(start, 0, 90*time.Millisecond, OutcomeServerError, 503),
		result(start, 0, 2*time.Second, OutcomeTimeout, 0),
	)

	if s.Total != 4 {
		t.Errorf("Total = %d, want 4", s.Total)
	}
	if s.Outcomes[OutcomeSuccess] != 2 || s.Outcomes[OutcomeServerError] != 1 || s.Outcomes[OutcomeTimeout] != 1 {
		t.Errorf("outcomes = %v", s.Outcomes)
	}
	if s.Statuses[200] != 2 || s.Statuses[503] != 1 {
		t.Errorf("statuses = %v", s.Statuses)
	}
	// A request that never got a response has no status code to report, and
	// recording a 0 would invent one.
	if _, ok := s.Statuses[0]; ok {
		t.Error("a zero status was recorded for a request that got no response")
	}
	if s.Failures() != 2 {
		t.Errorf("Failures() = %d, want 2", s.Failures())
	}
	if s.FailureRate() != 0.5 {
		t.Errorf("FailureRate() = %v, want 0.5", s.FailureRate())
	}
}

// Successful-request latency must exclude failures: a timeout at the full
// deadline would otherwise dominate the percentiles and make a healthy
// service look slow.
func TestCollectorSeparatesSuccessLatencyFromAll(t *testing.T) {
	start := time.Now()
	s := collect(start,
		result(start, 0, 10*time.Millisecond, OutcomeSuccess, 200),
		result(start, 0, 20*time.Millisecond, OutcomeSuccess, 200),
		result(start, 0, 5*time.Second, OutcomeTimeout, 0),
	)

	if s.Success.Count != 2 {
		t.Errorf("Success.Count = %d, want 2", s.Success.Count)
	}
	if s.Success.P99 >= time.Second {
		t.Errorf("Success.P99 = %v; the timeout leaked into successful latency", s.Success.P99)
	}
	if s.All.Count != 3 {
		t.Errorf("All.Count = %d, want 3", s.All.Count)
	}
	if s.All.P99 < time.Second {
		t.Errorf("All.P99 = %v; the timeout should be visible here", s.All.P99)
	}
}

// A reused keep-alive connection does no DNS lookup and no handshake, so
// those phases are legitimately zero. Averaging the zeros in would drag the
// median to nothing and hide a genuinely slow resolver.
func TestCollectorSkipsPhasesThatDidNotHappen(t *testing.T) {
	start := time.Now()
	cold := Result{
		Start: start, Latency: 50 * time.Millisecond, Outcome: OutcomeSuccess, Status: 200, Attempts: 1,
		Phases: Phases{
			DNS: 8 * time.Millisecond, Connect: 12 * time.Millisecond,
			TLS: 20 * time.Millisecond, TTFB: 45 * time.Millisecond,
			Body: 5 * time.Millisecond, Reused: false,
		},
	}
	warm := Result{
		Start: start, Latency: 20 * time.Millisecond, Outcome: OutcomeSuccess, Status: 200, Attempts: 1,
		Phases: Phases{TTFB: 18 * time.Millisecond, Body: 2 * time.Millisecond, Reused: true},
	}

	s := collect(start, cold, warm, warm, warm)

	if s.DNS.Count != 1 {
		t.Errorf("DNS.Count = %d, want 1 — only the cold request resolved a name", s.DNS.Count)
	}
	if s.DNS.P50 != 8*time.Millisecond {
		t.Errorf("DNS.P50 = %v, want 8ms undiluted by three zeros", s.DNS.P50)
	}
	if s.Connect.Count != 1 || s.TLS.Count != 1 {
		t.Errorf("Connect.Count = %d, TLS.Count = %d, want 1 each", s.Connect.Count, s.TLS.Count)
	}
	// TTFB and body happen on every request, reused or not.
	if s.TTFB.Count != 4 || s.Body.Count != 4 {
		t.Errorf("TTFB.Count = %d, Body.Count = %d, want 4 each", s.TTFB.Count, s.Body.Count)
	}
	if s.Reused != 3 {
		t.Errorf("Reused = %d, want 3", s.Reused)
	}
	if s.ReuseRate() != 0.75 {
		t.Errorf("ReuseRate() = %v, want 0.75", s.ReuseRate())
	}
}

func TestCollectorSumsAttemptsForAmplification(t *testing.T) {
	start := time.Now()
	retried := result(start, 0, 100*time.Millisecond, OutcomeSuccess, 200)
	retried.Attempts = 3
	plain := result(start, 0, 10*time.Millisecond, OutcomeSuccess, 200)

	s := collect(start, retried, plain)

	if s.Total != 2 {
		t.Errorf("Total = %d, want 2 — a retried request is still one request", s.Total)
	}
	if s.Attempts != 4 {
		t.Errorf("Attempts = %d, want 4", s.Attempts)
	}
	if s.Amplification() != 2 {
		t.Errorf("Amplification() = %v, want 2", s.Amplification())
	}
}

// A request the breaker shed never reached the target, so it must not be
// counted as traffic the target absorbed.
func TestCollectorCountsShedRequestsAsZeroAttempts(t *testing.T) {
	start := time.Now()
	shed := result(start, 0, 50*time.Microsecond, OutcomeCircuitOpen, 0)
	shed.Attempts = 0

	s := collect(start, shed, result(start, 0, 10*time.Millisecond, OutcomeSuccess, 200))

	if s.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 — the shed request was never sent", s.Attempts)
	}
	if s.Rate(OutcomeCircuitOpen) != 0.5 {
		t.Errorf("Rate(circuit_open) = %v, want 0.5", s.Rate(OutcomeCircuitOpen))
	}
}

func TestCollectorBucketsBySecond(t *testing.T) {
	start := time.Now()
	var results []Result
	// Three seconds of traffic, with failures only in the third.
	for second := range 3 {
		for range 10 {
			outcome, status := OutcomeSuccess, 200
			if second == 2 {
				outcome, status = OutcomeServerError, 503
			}
			results = append(results, result(start,
				time.Duration(second)*time.Second+100*time.Millisecond,
				20*time.Millisecond, outcome, status))
		}
	}

	s := collect(start, results...)

	if len(s.Buckets) != 3 {
		t.Fatalf("got %d buckets, want 3", len(s.Buckets))
	}
	for i, b := range s.Buckets {
		if b.Second != i {
			t.Errorf("bucket %d has Second = %d", i, b.Second)
		}
		if b.Count != 10 {
			t.Errorf("bucket %d has Count = %d, want 10", i, b.Count)
		}
		wantFailed := 0
		if i == 2 {
			wantFailed = 10
		}
		if b.Failed != wantFailed {
			t.Errorf("bucket %d has Failed = %d, want %d", i, b.Failed, wantFailed)
		}
	}
}

// Results that arrive slightly before the recorded start (clock jitter
// between the runner and a probe) must not produce a negative bucket index.
func TestCollectorClampsEarlyResultsIntoTheFirstBucket(t *testing.T) {
	start := time.Now()
	early := result(start, -50*time.Millisecond, 10*time.Millisecond, OutcomeSuccess, 200)

	s := collect(start, early)

	if len(s.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(s.Buckets))
	}
	if s.Buckets[0].Second != 0 {
		t.Errorf("Second = %d, want 0", s.Buckets[0].Second)
	}
}

func TestCollectorMeasuresElapsedFromTheLastResult(t *testing.T) {
	start := time.Now()
	s := collect(start,
		result(start, 0, 10*time.Millisecond, OutcomeSuccess, 200),
		result(start, 4*time.Second, 500*time.Millisecond, OutcomeSuccess, 200),
	)

	// The run ended when the last request finished: 4s in plus its 500ms.
	if got := s.Elapsed(); got < 4*time.Second || got > 5*time.Second {
		t.Errorf("Elapsed() = %v, want ~4.5s", got)
	}
	if s.Throughput() <= 0 {
		t.Error("throughput was not computed")
	}
}

func TestCollectorSumsBytes(t *testing.T) {
	start := time.Now()
	a := result(start, 0, time.Millisecond, OutcomeSuccess, 200)
	a.Bytes = 1024
	b := result(start, 0, time.Millisecond, OutcomeSuccess, 200)
	b.Bytes = 512

	if s := collect(start, a, b); s.Bytes != 1536 {
		t.Errorf("Bytes = %d, want 1536", s.Bytes)
	}
}

func TestCollectorLiveStatsTrackProgress(t *testing.T) {
	start := time.Now()
	c := NewCollector(start)
	ch := make(chan Result, 3)
	ch <- result(start, 0, time.Millisecond, OutcomeSuccess, 200)
	ch <- result(start, 0, time.Millisecond, OutcomeServerError, 503)
	ch <- result(start, 0, time.Millisecond, OutcomeTimeout, 0)
	close(ch)
	c.Collect(ch)

	live := c.Live()
	if live.Sent != 3 {
		t.Errorf("Sent = %d, want 3", live.Sent)
	}
	if live.Failed != 2 {
		t.Errorf("Failed = %d, want 2", live.Failed)
	}
}

// An empty run must produce a usable zero summary rather than dividing by it.
func TestCollectorEmptyRun(t *testing.T) {
	s := collect(time.Now())

	if s.Total != 0 {
		t.Errorf("Total = %d, want 0", s.Total)
	}
	for name, got := range map[string]float64{
		"FailureRate":   s.FailureRate(),
		"Throughput":    s.Throughput(),
		"ReuseRate":     s.ReuseRate(),
		"Amplification": s.Amplification(),
		"Rate":          s.Rate(OutcomeSuccess),
	} {
		if got != 0 {
			t.Errorf("%s() = %v on an empty run, want 0", name, got)
		}
	}
	if len(s.Buckets) != 0 {
		t.Errorf("got %d buckets, want none", len(s.Buckets))
	}
}
