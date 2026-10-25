package ramp

import "time"

// Analysis is what a sweep concluded. It is plain data so that the renderer
// decides wording and the thresholds stay testable.
type Analysis struct {
	// Peak is the step with the highest successful throughput — the service's
	// usable capacity.
	Peak Step
	// PeakIndex is Peak's position in Steps.
	PeakIndex int

	// BaseLatency is p50 at the lowest concurrency tested, which approximates
	// the service time of the work itself with nothing queueing.
	BaseLatency time.Duration
	// TopLatency is p50 at the highest concurrency tested.
	TopLatency time.Duration
	// LatencyInflation is TopLatency / BaseLatency.
	LatencyInflation float64
	// QueueShare is the fraction of a request at the top level that is spent
	// waiting rather than being served, 0..1.
	QueueShare float64

	// NegativeReturns is true when throughput at the highest level fell
	// meaningfully below the peak: adding load made the service do less work.
	NegativeReturns bool
	// ThroughputDrop is how far the top level fell below peak, 0..1.
	ThroughputDrop float64

	// Saturated is true when the sweep found a limit at all. False means
	// throughput was still climbing at the last level, so the real limit is
	// higher than anything tested.
	Saturated bool

	// FirstFailureAt is the lowest concurrency where the failure rate passed
	// failureThreshold, or 0 if the service never failed.
	FirstFailureAt int

	// SLO, when set, is the p99 budget. SLOLimit is the highest concurrency
	// that met it, and SLOThroughput the throughput there.
	SLO           time.Duration
	SLOLimit      int
	SLOThroughput float64
	SLONeverMet   bool
}

const (
	// A top-level throughput this far below peak counts as negative returns
	// rather than measurement noise.
	negativeReturnsThreshold = 0.10
	// Throughput within this of peak counts as a plateau, not growth.
	plateauThreshold = 0.05
	failureThreshold = 0.01
	inflationFloor   = 1.0
)

// Analyze finds the knee of the throughput curve and the consequences of
// pushing past it.
func Analyze(r *Result, slo time.Duration) Analysis {
	a := Analysis{SLO: slo}
	if len(r.Steps) == 0 {
		return a
	}

	a.Peak = r.Steps[0]
	for i, step := range r.Steps[1:] {
		if step.Summary.Throughput() > a.Peak.Summary.Throughput() {
			a.Peak = step
			a.PeakIndex = i + 1
		}
	}

	first := r.Steps[0]
	last := r.Steps[len(r.Steps)-1]
	a.BaseLatency = first.Summary.Success.P50
	a.TopLatency = last.Summary.Success.P50
	if a.BaseLatency > 0 && a.TopLatency > 0 {
		a.LatencyInflation = float64(a.TopLatency) / float64(a.BaseLatency)
		if a.LatencyInflation > inflationFloor {
			a.QueueShare = 1 - float64(a.BaseLatency)/float64(a.TopLatency)
		}
	}

	peakThroughput := a.Peak.Summary.Throughput()
	topThroughput := last.Summary.Throughput()
	if peakThroughput > 0 {
		if drop := (peakThroughput - topThroughput) / peakThroughput; drop > negativeReturnsThreshold {
			a.NegativeReturns = true
			a.ThroughputDrop = drop
		}
	}

	// The sweep found a limit if the peak was not the final level, or if the
	// final level merely matched the peak instead of exceeding it.
	if a.PeakIndex < len(r.Steps)-1 {
		a.Saturated = true
	} else if len(r.Steps) > 1 {
		previous := r.Steps[len(r.Steps)-2].Summary.Throughput()
		if previous > 0 && (topThroughput-previous)/previous < plateauThreshold {
			a.Saturated = true
		}
	}

	for _, step := range r.Steps {
		if step.Summary.FailureRate() > failureThreshold {
			a.FirstFailureAt = step.Concurrency
			break
		}
	}

	if slo > 0 {
		a.SLONeverMet = true
		for _, step := range r.Steps {
			if step.Summary.Success.P99 > 0 && step.Summary.Success.P99 <= slo &&
				step.Summary.FailureRate() <= failureThreshold {
				a.SLOLimit = step.Concurrency
				a.SLOThroughput = step.Summary.Throughput()
				a.SLONeverMet = false
			}
		}
	}

	return a
}
