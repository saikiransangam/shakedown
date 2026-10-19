package metrics

import (
	"math"
	"slices"
	"time"
)

// Distribution is a summary of a set of latency samples.
type Distribution struct {
	Count  int
	Min    time.Duration
	Max    time.Duration
	Mean   time.Duration
	StdDev time.Duration
	P50    time.Duration
	P90    time.Duration
	P95    time.Duration
	P99    time.Duration
	P999   time.Duration
}

// NewDistribution computes exact percentiles by sorting a copy of samples.
//
// Keeping every sample costs 8 bytes per request, so a million requests is 8MB
// — cheap enough that we never need the approximation an HDR histogram would
// give us, and the numbers are exact rather than bucketed.
func NewDistribution(samples []time.Duration) Distribution {
	d := Distribution{Count: len(samples)}
	if d.Count == 0 {
		return d
	}

	sorted := slices.Clone(samples)
	slices.Sort(sorted)

	var sum float64
	for _, v := range sorted {
		sum += float64(v)
	}
	mean := sum / float64(len(sorted))

	var sumSq float64
	for _, v := range sorted {
		diff := float64(v) - mean
		sumSq += diff * diff
	}

	d.Min = sorted[0]
	d.Max = sorted[len(sorted)-1]
	d.Mean = time.Duration(mean)
	d.StdDev = time.Duration(math.Sqrt(sumSq / float64(len(sorted))))
	d.P50 = Percentile(sorted, 50)
	d.P90 = Percentile(sorted, 90)
	d.P95 = Percentile(sorted, 95)
	d.P99 = Percentile(sorted, 99)
	d.P999 = Percentile(sorted, 99.9)
	return d
}

// Percentile returns the p-th percentile of an already-sorted slice using the
// nearest-rank method, which is the convention load generators report and
// which never invents a value that was not actually measured.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// TailRatio reports how much worse the slow requests are than the typical one.
// A healthy service sits under ~5; past ~10 something is queueing.
func (d Distribution) TailRatio() float64 {
	if d.P50 <= 0 {
		return 0
	}
	return float64(d.P99) / float64(d.P50)
}
