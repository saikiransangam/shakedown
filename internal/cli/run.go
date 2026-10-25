package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/saikiransangam/shakedown/breaker"
	"github.com/saikiransangam/shakedown/internal/report"
	"github.com/saikiransangam/shakedown/load"
	"github.com/saikiransangam/shakedown/metrics"
	"github.com/saikiransangam/shakedown/probe"
	"github.com/saikiransangam/shakedown/resilience"
	"github.com/saikiransangam/shakedown/retry"
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

		retries     = fs.Int("retries", 1, "max attempts per request (1 = no retries)")
		backoff     = fs.String("backoff", "exponential", "backoff strategy: fixed or exponential")
		jitter      = fs.String("jitter", "full", "jitter mode: none, full or equal")
		baseDelay   = fs.Duration("base-delay", 50*time.Millisecond, "first retry delay")
		maxDelay    = fs.Duration("max-delay", 2*time.Second, "cap on retry delay")
		retryBudget = fs.Float64("retry-budget", 0, "cap retries at this share of traffic (0 = uncapped)")

		useBreaker       = fs.Bool("breaker", false, "enable a circuit breaker")
		breakerThreshold = fs.Float64("breaker-threshold", 0.5, "failure ratio that opens the breaker")
		breakerMin       = fs.Int("breaker-min-requests", 20, "minimum requests in the window before it can open")
		breakerWindow    = fs.Duration("breaker-window", 10*time.Second, "rolling window for the failure ratio")
		breakerCooldown  = fs.Duration("breaker-cooldown", 2*time.Second, "how long to stay open before probing")
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

	var probeUnderTest load.Probe = httpProbe
	var policyInfo *report.PolicyInfo
	var circuitBreaker *breaker.Breaker

	if *retries > 1 || *useBreaker {
		strategy, err := retry.ParseStrategy(*backoff)
		if err != nil {
			return 2, err
		}
		jitterMode, err := retry.ParseJitter(*jitter)
		if err != nil {
			return 2, err
		}

		policy := retry.Policy{
			MaxAttempts: *retries,
			BaseDelay:   *baseDelay,
			MaxDelay:    *maxDelay,
			Strategy:    strategy,
			Jitter:      jitterMode,
		}
		if *retryBudget > 0 {
			policy.Budget = retry.NewBudget(*retryBudget)
		}

		if *useBreaker {
			circuitBreaker = breaker.New(breaker.Config{
				FailureThreshold: *breakerThreshold,
				MinRequests:      *breakerMin,
				Window:           *breakerWindow,
				Cooldown:         *breakerCooldown,
			})
		}

		probeUnderTest = resilience.Wrap(httpProbe, policy, circuitBreaker)
		policyInfo = &report.PolicyInfo{
			MaxAttempts: *retries,
			Strategy:    string(strategy),
			Jitter:      string(jitterMode),
			BudgetRatio: *retryBudget,
		}
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

	summary := load.Run(ctx, cfg, probeUnderTest)
	summary.Target = *target

	if circuitBreaker != nil {
		stats := circuitBreaker.Stats()
		policyInfo.Breaker = &stats
	}

	if showProgress {
		fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", 60))
	}
	report.Text(os.Stdout, summary, policyInfo)

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
