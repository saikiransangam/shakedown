package load

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

// Probe is one unit of work under test. Keeping the runner ignorant of HTTP
// means the same concurrency engine can drive a gRPC call, a database query or
// a fake probe in a test.
type Probe interface {
	Do(ctx context.Context) metrics.Result
}

// ProbeFunc adapts a plain function to Probe.
type ProbeFunc func(ctx context.Context) metrics.Result

func (f ProbeFunc) Do(ctx context.Context) metrics.Result { return f(ctx) }

type Config struct {
	// Concurrency is the number of workers sending requests in parallel.
	Concurrency int
	// Duration bounds the run by wall clock. Ignored when Requests > 0.
	Duration time.Duration
	// Requests bounds the run by a total request count. 0 means duration-bound.
	Requests int
	// Rate caps requests per second across all workers. 0 means as fast as
	// the workers can go, which measures capacity rather than behaviour at a
	// known load.
	Rate float64

	// OnTick, if set, is called periodically with a progress snapshot.
	OnTick    func(metrics.LiveStats)
	TickEvery time.Duration
}

const defaultDuration = 10 * time.Second

// Run drives the probe under the given config and returns the aggregated
// summary. It stops early if ctx is canceled, and still returns everything
// measured up to that point.
func Run(ctx context.Context, cfg Config, p Probe) *metrics.Summary {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Duration <= 0 && cfg.Requests <= 0 {
		cfg.Duration = defaultDuration
	}

	// runCtx ends when the run is over; ctx ending means the *user* stopped us,
	// which we report differently.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.Requests <= 0 && cfg.Duration > 0 {
		timer := time.AfterFunc(cfg.Duration, cancel)
		defer timer.Stop()
	}

	start := time.Now()
	collector := metrics.NewCollector(start)
	results := make(chan metrics.Result, cfg.Concurrency*4)

	done := make(chan *metrics.Summary, 1)
	go func() { done <- collector.Collect(results) }()

	stopProgress := make(chan struct{})
	if cfg.OnTick != nil {
		every := cfg.TickEvery
		if every <= 0 {
			every = 500 * time.Millisecond
		}
		go func() {
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					cfg.OnTick(collector.Live())
				case <-stopProgress:
					return
				}
			}
		}()
	}

	// One ticker shared by every worker is a global rate limit: a tick is
	// delivered to exactly one receiver. Ticks are dropped when no worker is
	// waiting, which is what we want — a stalled target must not build up a
	// burst of credit that fires all at once when it recovers.
	var pace <-chan time.Time
	if cfg.Rate > 0 {
		ticker := time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
		defer ticker.Stop()
		pace = ticker.C
	}

	var remaining atomic.Int64
	if cfg.Requests > 0 {
		remaining.Store(int64(cfg.Requests))
	}

	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if runCtx.Err() != nil {
					return
				}
				if cfg.Requests > 0 && remaining.Add(-1) < 0 {
					return
				}
				if pace != nil {
					select {
					case <-pace:
					case <-runCtx.Done():
						return
					}
				}

				res := p.Do(runCtx)

				// A cancellation we caused ourselves (the clock ran out, or
				// Ctrl-C) says nothing about the target, so it must not land
				// in the error taxonomy as if the server had failed.
				if res.Outcome == metrics.OutcomeCanceled && runCtx.Err() != nil {
					return
				}
				results <- res
			}
		}()
	}

	wg.Wait()
	close(stopProgress)
	close(results)

	summary := <-done
	summary.Concurrency = cfg.Concurrency
	summary.RateLimit = cfg.Rate
	summary.Interrupted = ctx.Err() != nil
	return summary
}
