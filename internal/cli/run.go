package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/saikiransangam/shakedown/internal/report"
	"github.com/saikiransangam/shakedown/load"
	"github.com/saikiransangam/shakedown/metrics"
	"github.com/saikiransangam/shakedown/probe"
)

// Run executes the `shakedown run` subcommand. It returns the process exit
// code so CI can gate on a failure budget.
func Run(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: shakedown run --target URL [flags]\n\n")
		fs.PrintDefaults()
	}

	var (
		target      = fs.String("target", "", "URL to test (required)")
		concurrency = fs.Int("c", 10, "number of concurrent workers")
		duration    = fs.Duration("d", 10*time.Second, "how long to run")
		requests    = fs.Int("n", 0, "stop after this many requests (overrides -d)")
		rate        = fs.Float64("rate", 0, "cap total requests per second (0 = unthrottled)")
		timeout     = fs.Duration("timeout", 5*time.Second, "per-request timeout")
		method      = fs.String("method", "GET", "HTTP method")
		body        = fs.String("body", "", "request body")
		insecure    = fs.Bool("insecure", false, "skip TLS certificate verification")
		noKeepAlive = fs.Bool("no-keepalive", false, "open a fresh connection for every request")
		redirects   = fs.Bool("follow-redirects", false, "follow 3xx redirects")
		maxFailRate = fs.Float64("max-failure-rate", -1, "exit non-zero if the failure rate exceeds this (0..1)")
		quiet       = fs.Bool("quiet", false, "suppress live progress output")
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

	httpProbe, err := probe.NewHTTP(probe.HTTPOptions{
		Method:            *method,
		URL:               *target,
		Header:            headers.header,
		Body:              []byte(*body),
		Timeout:           *timeout,
		PoolSize:          *concurrency,
		Insecure:          *insecure,
		DisableKeepAlives: *noKeepAlive,
		FollowRedirects:   *redirects,
	})
	if err != nil {
		return 1, err
	}

	cfg := load.Config{
		Concurrency: *concurrency,
		Duration:    *duration,
		Requests:    *requests,
		Rate:        *rate,
	}
	// Progress rewrites one line with \r, which only makes sense on a
	// terminal; piped into a file or CI log it would be thousands of lines.
	showProgress := !*quiet && report.IsTerminal(os.Stderr)
	if showProgress {
		cfg.OnTick = progressPrinter()
	}

	plan := fmt.Sprintf("%d workers, %s", *concurrency, *duration)
	if *requests > 0 {
		plan = fmt.Sprintf("%d workers, %d requests", *concurrency, *requests)
	}
	fmt.Fprintf(os.Stderr, "running %s against %s (timeout %s)\n", plan, *target, *timeout)

	summary := load.Run(ctx, cfg, httpProbe)
	summary.Target = *target

	if showProgress {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 60))
	}
	report.Text(os.Stdout, summary)

	if *maxFailRate >= 0 && summary.FailureRate() > *maxFailRate {
		fmt.Fprintf(os.Stderr, "failure rate %.2f%% exceeds budget %.2f%%\n",
			summary.FailureRate()*100, *maxFailRate*100)
		return 1, nil
	}
	return 0, nil
}

func progressPrinter() func(metrics.LiveStats) {
	return func(live metrics.LiveStats) {
		rate := 0.0
		if secs := live.Elapsed.Seconds(); secs > 0 {
			rate = float64(live.Sent) / secs
		}
		fmt.Fprintf(os.Stderr, "\r  %5.1fs  %7d sent  %6.0f req/s  %d failed   ",
			live.Elapsed.Seconds(), live.Sent, rate, live.Failed)
	}
}
