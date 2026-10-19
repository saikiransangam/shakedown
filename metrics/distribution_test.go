package metrics

import (
	"testing"
	"time"
)

func TestNewDistributionEmpty(t *testing.T) {
	d := NewDistribution(nil)
	if d.Count != 0 || d.P50 != 0 {
		t.Fatalf("empty distribution should be zero, got %+v", d)
	}
}

func TestNewDistributionPercentiles(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}

	d := NewDistribution(samples)

	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"min", d.Min, 1 * time.Millisecond},
		{"max", d.Max, 100 * time.Millisecond},
		{"p50", d.P50, 50 * time.Millisecond},
		{"p90", d.P90, 90 * time.Millisecond},
		{"p99", d.P99, 99 * time.Millisecond},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if d.Count != 100 {
		t.Errorf("count = %d, want 100", d.Count)
	}
}

func TestNewDistributionDoesNotMutateInput(t *testing.T) {
	samples := []time.Duration{3, 1, 2}
	NewDistribution(samples)
	if samples[0] != 3 {
		t.Fatalf("input was sorted in place: %v", samples)
	}
}

func TestPercentileNeverInventsValues(t *testing.T) {
	sorted := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if got := Percentile(sorted, 75); got != 20*time.Millisecond {
		t.Errorf("p75 = %v, want 20ms", got)
	}
	if got := Percentile(sorted, 1); got != 10*time.Millisecond {
		t.Errorf("p1 = %v, want 10ms", got)
	}
}

func TestTailRatio(t *testing.T) {
	d := Distribution{P50: 10 * time.Millisecond, P99: 250 * time.Millisecond}
	if got := d.TailRatio(); got != 25 {
		t.Errorf("tail ratio = %v, want 25", got)
	}
	if got := (Distribution{}).TailRatio(); got != 0 {
		t.Errorf("empty tail ratio = %v, want 0", got)
	}
}
