package load

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

func countingProbe(calls *atomic.Int64, latency time.Duration) Probe {
	return ProbeFunc(func(ctx context.Context) metrics.Result {
		calls.Add(1)
		start := time.Now()
		if latency > 0 {
			select {
			case <-time.After(latency):
			case <-ctx.Done():
				return metrics.Result{Start: start, Latency: time.Since(start), Outcome: metrics.OutcomeCanceled}
			}
		}
		return metrics.Result{Start: start, Latency: time.Since(start), Status: 200, Outcome: metrics.OutcomeSuccess}
	})
}

func TestRunStopsAtRequestCount(t *testing.T) {
	var calls atomic.Int64
	summary := Run(context.Background(), Config{Concurrency: 8, Requests: 200}, countingProbe(&calls, 0))

	if summary.Total != 200 {
		t.Errorf("recorded %d results, want 200", summary.Total)
	}
	if got := calls.Load(); got != 200 {
		t.Errorf("probe called %d times, want 200", got)
	}
	if summary.Concurrency != 8 {
		t.Errorf("concurrency = %d, want 8", summary.Concurrency)
	}
}

func TestRunStopsAtDuration(t *testing.T) {
	var calls atomic.Int64
	start := time.Now()
	summary := Run(context.Background(), Config{Concurrency: 4, Duration: 250 * time.Millisecond}, countingProbe(&calls, 10*time.Millisecond))
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("run took %v, should have stopped near 250ms", elapsed)
	}
	if summary.Total == 0 {
		t.Error("expected at least one result")
	}
	if summary.Interrupted {
		t.Error("a run that hit its own deadline is not interrupted")
	}
}

func TestRunRespectsRateLimit(t *testing.T) {
	var calls atomic.Int64
	summary := Run(context.Background(), Config{
		Concurrency: 16,
		Duration:    500 * time.Millisecond,
		Rate:        20,
	}, countingProbe(&calls, 0))

	// 20/s for half a second is ~10 requests; allow generous slack for timer
	// granularity, but 16 unthrottled workers would produce thousands.
	if summary.Total > 60 {
		t.Errorf("rate limit ignored: %d requests in 500ms at 20/s", summary.Total)
	}
	if summary.Total == 0 {
		t.Error("rate limiter starved the run completely")
	}
}

func TestRunCanceledByCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	summary := Run(ctx, Config{Concurrency: 4, Duration: 10 * time.Second}, countingProbe(&calls, 5*time.Millisecond))

	if !summary.Interrupted {
		t.Error("expected the summary to be marked interrupted")
	}
	// Cancellations we caused must not be reported as target failures.
	if n := summary.Outcomes[metrics.OutcomeCanceled]; n != 0 {
		t.Errorf("recorded %d canceled results, want 0", n)
	}
}

func TestRunDefaultsToOneWorker(t *testing.T) {
	var calls atomic.Int64
	summary := Run(context.Background(), Config{Requests: 5}, countingProbe(&calls, 0))
	if summary.Concurrency != 1 {
		t.Errorf("concurrency = %d, want 1", summary.Concurrency)
	}
	if summary.Total != 5 {
		t.Errorf("total = %d, want 5", summary.Total)
	}
}
