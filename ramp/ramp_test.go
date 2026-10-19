package ramp

import (
	"slices"
	"testing"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

func TestGeometricLevels(t *testing.T) {
	cases := []struct {
		from, to int
		want     []int
	}{
		{1, 64, []int{1, 2, 4, 8, 16, 32, 64}},
		{1, 100, []int{1, 2, 4, 8, 16, 32, 64, 100}},
		{10, 10, []int{10}},
		{4, 16, []int{4, 8, 16}},
		{0, 2, []int{1, 2}},
		{50, 10, []int{50}},
	}
	for _, c := range cases {
		got := GeometricLevels(c.from, c.to)
		if !slices.Equal(got, c.want) {
			t.Errorf("GeometricLevels(%d, %d) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestParseLevels(t *testing.T) {
	got, err := ParseLevels(" 1, 10 ,50")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{1, 10, 50}) {
		t.Errorf("got %v", got)
	}

	for _, bad := range []string{"", "abc", "0", "-5", ","} {
		if _, err := ParseLevels(bad); err == nil {
			t.Errorf("ParseLevels(%q) should have failed", bad)
		}
	}
}

// step builds a synthetic step so the analysis can be tested without a server.
func step(concurrency int, throughput float64, p50, p99 time.Duration, failureRate float64) Step {
	const window = 10 * time.Second
	total := int(throughput * window.Seconds())
	failures := int(float64(total) * failureRate)

	start := time.Now()
	return Step{
		Concurrency: concurrency,
		Summary: &metrics.Summary{
			Concurrency: concurrency,
			Start:       start,
			End:         start.Add(window),
			Total:       total,
			Outcomes: map[metrics.Outcome]int{
				metrics.OutcomeSuccess:     total - failures,
				metrics.OutcomeServerError: failures,
			},
			Success: metrics.Distribution{Count: total - failures, P50: p50, P99: p99},
		},
	}
}

func TestAnalyzeFindsPeakAndNegativeReturns(t *testing.T) {
	result := &Result{Steps: []Step{
		step(1, 50, 20*time.Millisecond, 30*time.Millisecond, 0),
		step(10, 490, 20*time.Millisecond, 31*time.Millisecond, 0),
		step(40, 300, 130*time.Millisecond, 140*time.Millisecond, 0),
		step(80, 140, 580*time.Millisecond, 590*time.Millisecond, 0),
	}}

	a := Analyze(result, 0)

	if a.Peak.Concurrency != 10 {
		t.Errorf("peak concurrency = %d, want 10", a.Peak.Concurrency)
	}
	if !a.Saturated {
		t.Error("expected the sweep to be recognised as saturated")
	}
	if !a.NegativeReturns {
		t.Error("throughput fell from 490 to 140; expected negative returns")
	}
	if a.LatencyInflation != 29 {
		t.Errorf("latency inflation = %v, want 29", a.LatencyInflation)
	}
	// 20ms of a 580ms request is service time, so ~96.5% is queueing.
	if a.QueueShare < 0.96 || a.QueueShare > 0.97 {
		t.Errorf("queue share = %v, want ~0.966", a.QueueShare)
	}
	if a.FirstFailureAt != 0 {
		t.Errorf("no step failed, but FirstFailureAt = %d", a.FirstFailureAt)
	}
}

func TestAnalyzeUnsaturated(t *testing.T) {
	result := &Result{Steps: []Step{
		step(1, 50, 20*time.Millisecond, 30*time.Millisecond, 0),
		step(2, 100, 20*time.Millisecond, 30*time.Millisecond, 0),
		step(4, 200, 20*time.Millisecond, 30*time.Millisecond, 0),
	}}

	a := Analyze(result, 0)

	if a.Saturated {
		t.Error("throughput doubled at every level; the limit was not reached")
	}
	if a.Peak.Concurrency != 4 {
		t.Errorf("peak = %d, want 4", a.Peak.Concurrency)
	}
	if a.NegativeReturns {
		t.Error("throughput never fell")
	}
}

func TestAnalyzeFirstFailureAndSLO(t *testing.T) {
	result := &Result{Steps: []Step{
		step(1, 50, 20*time.Millisecond, 40*time.Millisecond, 0),
		step(10, 400, 60*time.Millisecond, 150*time.Millisecond, 0),
		step(40, 420, 200*time.Millisecond, 400*time.Millisecond, 0.05),
	}}

	a := Analyze(result, 200*time.Millisecond)

	if a.FirstFailureAt != 40 {
		t.Errorf("FirstFailureAt = %d, want 40", a.FirstFailureAt)
	}
	// Level 40 breaches both the p99 budget and the failure threshold.
	if a.SLOLimit != 10 {
		t.Errorf("SLOLimit = %d, want 10", a.SLOLimit)
	}
	if a.SLONeverMet {
		t.Error("the budget was met at lower levels")
	}
}

func TestAnalyzeSLONeverMet(t *testing.T) {
	result := &Result{Steps: []Step{
		step(1, 50, 400*time.Millisecond, 900*time.Millisecond, 0),
	}}

	a := Analyze(result, 100*time.Millisecond)

	if !a.SLONeverMet {
		t.Error("p99 was 900ms against a 100ms budget")
	}
	if a.SLOLimit != 0 {
		t.Errorf("SLOLimit = %d, want 0", a.SLOLimit)
	}
}

func TestAnalyzeEmpty(t *testing.T) {
	a := Analyze(&Result{}, 0)
	if a.Peak.Concurrency != 0 || a.Saturated {
		t.Errorf("empty result should analyse to zero, got %+v", a)
	}
}
