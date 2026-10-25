package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/saikiransangam/shakedown/internal/report"
	"github.com/saikiransangam/shakedown/load"
	"github.com/saikiransangam/shakedown/probe"
	"github.com/saikiransangam/shakedown/ramp"
)

// Ramp executes the `shakedown ramp` subcommand.
func Ramp(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ramp", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: shakedown ramp --target URL [flags]\n\n")
		fs.PrintDefaults()
	}

	var (
		target    = fs.String("target", "", "URL to test (required)")
		from      = fs.Int("from", 1, "lowest concurrency level")
		to        = fs.Int("to", 64, "highest concurrency level")
		levelSpec = fs.String("levels", "", "explicit comma-separated levels, e.g. 1,10,50 (overrides --from/--to)")
		stepTime  = fs.Duration("step-duration", 5*time.Second, "how long to measure each level")
		warmup    = fs.Duration("warmup", time.Second, "unrecorded load before each level")
		timeout   = fs.Duration("timeout", 5*time.Second, "per-request timeout")
		slo       = fs.Duration("slo", 0, "p99 latency budget; reports the highest concurrency that meets it")
		method    = fs.String("method", "GET", "HTTP method")
		body      = fs.String("body", "", "request body")
		insecure  = fs.Bool("insecure", false, "skip TLS certificate verification")
	)
	headers := &headerFlag{}
	fs.Var(headers, "H", "request header in 'Name: value' form (repeatable)")

	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if *target == "" {
		fs.Usage()
		return 2, fmt.Errorf("--target is required")
	}

	levels := ramp.GeometricLevels(*from, *to)
	if *levelSpec != "" {
		parsed, err := ramp.ParseLevels(*levelSpec)
		if err != nil {
			return 2, err
		}
		levels = parsed
	}

	newProbe := func(concurrency int) (load.Probe, error) {
		return probe.NewHTTP(probe.HTTPOptions{
			Method:   *method,
			URL:      *target,
			Header:   headers.header,
			Body:     []byte(*body),
			Timeout:  *timeout,
			PoolSize: concurrency,
			Insecure: *insecure,
		})
	}

	estimate := time.Duration(len(levels)) * (*stepTime + *warmup)
	fmt.Fprintf(os.Stderr, "ramping %s across %v (about %s)\n", *target, levels, estimate.Round(time.Second))

	onStep := func(step ramp.Step) {
		s := step.Summary
		fmt.Fprintf(os.Stderr, "  c=%-4d %8.1f req/s   p50 %-8s p99 %-8s %s fail\n",
			step.Concurrency, s.Throughput(),
			report.Dur(s.Success.P50), report.Dur(s.Success.P99),
			report.Pct(s.FailureRate()))
	}

	result, err := ramp.Run(ctx, ramp.Config{
		Levels:       levels,
		StepDuration: *stepTime,
		Warmup:       *warmup,
	}, newProbe, onStep)
	if err != nil {
		return 1, err
	}
	result.Target = *target

	report.Ramp(os.Stdout, result, ramp.Analyze(result, *slo))
	return 0, nil
}
