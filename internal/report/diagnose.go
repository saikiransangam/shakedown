package report

import (
	"fmt"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarn
	SeverityBad
)

// Finding is one conclusion drawn from a run: what we saw, and what it usually
// means. The second half is the part that is hard to get from a latency table.
type Finding struct {
	Severity Severity
	Title    string
	Detail   string
}

const (
	tailRatioThreshold     = 10.0
	timeoutRateThreshold   = 0.01
	errorRateThreshold     = 0.01
	driftRatioThreshold    = 2.0
	serverBoundTTFBShare   = 0.8
	slowConnectThreshold   = 50 * time.Millisecond
	lowReuseRateThreshold  = 0.5
	amplificationThreshold = 1.2
	retryStormFailureRate  = 0.25
)

// Diagnose turns a summary into findings, ordered most severe first.
func Diagnose(s *metrics.Summary) []Finding {
	var findings []Finding

	if s.Total == 0 {
		return []Finding{{
			Severity: SeverityBad,
			Title:    "no requests completed",
			Detail:   "Nothing reached the target. Check the URL, and that something is listening on that port.",
		}}
	}

	findings = append(findings, diagnoseConnectivity(s)...)
	findings = append(findings, diagnoseErrors(s)...)
	findings = append(findings, diagnoseLatency(s)...)
	findings = append(findings, diagnoseDrift(s)...)
	findings = append(findings, diagnosePhases(s)...)
	findings = append(findings, diagnosePolicies(s)...)

	if len(findings) == 0 {
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Title:    "no reliability problems detected at this load",
			Detail: fmt.Sprintf("The target held up at %d concurrent requests. Raise concurrency until it doesn't, "+
				"or inject faults with `shakedown mock --profile brownout` to see what failure looks like.", s.Concurrency),
		})
	}
	return findings
}

func diagnoseConnectivity(s *metrics.Summary) []Finding {
	var out []Finding

	if n := s.Outcomes[metrics.OutcomeConnRefused]; n > 0 {
		out = append(out, Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("%s requests were refused outright (%s)", Count(n), Pct(s.Rate(metrics.OutcomeConnRefused))),
			Detail: "Nothing accepted the connection. Either the process died mid-run, or its accept queue " +
				"overflowed — the kernel backlog (somaxconn / the listener's backlog argument) drops new " +
				"connections once the application stops calling accept() fast enough.",
		})
	}

	if n := s.Outcomes[metrics.OutcomeConnReset]; n > 0 {
		out = append(out, Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("%s connections died mid-request (%s)", Count(n), Pct(s.Rate(metrics.OutcomeConnReset))),
			Detail: "The connection was established and then torn down before the response finished. Usual " +
				"causes: the server restarted or crashed, a proxy or load balancer closed an idle keep-alive " +
				"connection that the client still considered usable, or the server hit a per-connection limit.",
		})
	}

	if n := s.Outcomes[metrics.OutcomeDNSFailure]; n > 0 {
		out = append(out, Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("%s DNS lookups failed", Count(n)),
			Detail:   "Name resolution is failing intermittently, which will look like a random outage to every client of this service.",
		})
	}

	if n := s.Outcomes[metrics.OutcomeTLSFailure]; n > 0 {
		out = append(out, Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("%s TLS handshakes failed", Count(n)),
			Detail:   "Certificate or handshake failure. Use --insecure to confirm it is certificate validation and not a transport problem.",
		})
	}

	return out
}

func diagnoseErrors(s *metrics.Summary) []Finding {
	var out []Finding

	if rate := s.Rate(metrics.OutcomeTimeout); rate > timeoutRateThreshold {
		out = append(out, Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("%s of requests timed out", Pct(rate)),
			Detail: "The server accepted the request and never answered in time. That is usually a dependency " +
				"that has no timeout of its own, a lock or connection pool that requests are queued behind, or " +
				"simply more load than it has capacity for.",
		})
	}

	if rate := s.Rate(metrics.OutcomeServerError); rate > errorRateThreshold {
		finding := Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("%s of requests returned 5xx", Pct(rate)),
			Detail:   "The server knew it was failing. Check its logs for the same window.",
		}
		if early, late, ok := failureRateByHalf(s); ok && late > early*driftRatioThreshold {
			finding.Detail = fmt.Sprintf("Failures grew from %s in the first half of the run to %s in the second. "+
				"A failure rate that climbs under sustained load points at something being exhausted — connections, "+
				"memory, file descriptors, a thread pool — rather than a bug that fires on every request.",
				Pct(early), Pct(late))
		}
		out = append(out, finding)
	}

	if rate := s.Rate(metrics.OutcomeClientError); rate > 0.5 {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Title:    fmt.Sprintf("%s of requests returned 4xx", Pct(rate)),
			Detail:   "Mostly client errors — the request itself is probably wrong (auth, path, method), so this run is not measuring the happy path.",
		})
	}

	return out
}

func diagnoseLatency(s *metrics.Summary) []Finding {
	var out []Finding
	d := s.Success
	if d.Count < 20 {
		return out
	}

	if ratio := d.TailRatio(); ratio > tailRatioThreshold {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Title:    fmt.Sprintf("long tail: p99 (%s) is %.0fx p50 (%s)", Dur(d.P99), ratio, Dur(d.P50)),
			Detail: "A typical request is fine and the slowest are not, so the work itself is not slow — requests " +
				"are waiting. Queueing behind a saturated resource, garbage-collection or runtime pauses, and a " +
				"cold cache or connection pool all produce exactly this shape.",
		})
	}

	return out
}

func diagnoseDrift(s *metrics.Summary) []Finding {
	var out []Finding

	early, late, ok := medianByQuarter(s)
	if !ok {
		return out
	}
	if late > time.Duration(float64(early)*driftRatioThreshold) {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Title:    fmt.Sprintf("latency degraded during the run: %s -> %s (median)", Dur(early), Dur(late)),
			Detail: "The target got slower the longer it was under load, so the numbers here are a floor rather " +
				"than a steady state. Leaks, unbounded queues and caches that stop fitting all look like this. " +
				"Run for longer to find where it levels off.",
		})
	}

	return out
}

func diagnosePhases(s *metrics.Summary) []Finding {
	var out []Finding

	if s.TTFB.Count > 20 && s.Success.P50 > 0 {
		share := float64(s.TTFB.P50) / float64(s.Success.P50)
		if share > serverBoundTTFBShare && s.Success.P50 > 50*time.Millisecond {
			out = append(out, Finding{
				Severity: SeverityInfo,
				Title:    fmt.Sprintf("%s of the median request is spent waiting for the first byte", Pct(share)),
				Detail:   "Time is going into the server thinking, not into the network or transferring the body. Profile the handler, not the connection.",
			})
		}
	}

	if s.Connect.Count > 20 && s.Connect.P50 > slowConnectThreshold {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Title:    fmt.Sprintf("connection setup is slow: %s median", Dur(s.Connect.P50)),
			Detail:   "Establishing the TCP connection is a real share of each request. If the reuse rate is also low, the client is reconnecting constantly and measuring handshakes instead of the service.",
		})
	}

	if s.Total > 100 && s.ReuseRate() < lowReuseRateThreshold && s.Connect.Count > 20 {
		out = append(out, Finding{
			Severity: SeverityWarn,
			Title:    fmt.Sprintf("only %s of requests reused a connection", Pct(s.ReuseRate())),
			Detail:   "Most requests paid for a new TCP (and TLS) handshake. Either the server is closing connections (Connection: close, or a short idle timeout) or the client pool is too small for this concurrency.",
		})
	}

	return out
}

func diagnosePolicies(s *metrics.Summary) []Finding {
	var out []Finding

	amp := s.Amplification()
	if amp >= amplificationThreshold {
		extra := s.Attempts - s.Total
		finding := Finding{
			Severity: SeverityWarn,
			Title:    fmt.Sprintf("retries amplified load %.2fx (%s extra attempts)", amp, Count(extra)),
			Detail: "Retries are doing their job, but note that the target absorbed more traffic than this " +
				"run's request count suggests. Size capacity against attempts, not requests.",
		}
		// Retrying into a target that is failing most requests is the classic
		// retry storm: the extra load is the reason it cannot recover.
		if s.FailureRate() > retryStormFailureRate {
			finding.Severity = SeverityBad
			finding.Title = fmt.Sprintf("retry storm: %.2fx load against a target failing %s of requests",
				amp, Pct(s.FailureRate()))
			finding.Detail = "The target is failing and every failure is being retried, so it is receiving " +
				"several times its normal load precisely when it has least capacity. This is self-sustaining — " +
				"the retries cause the failures that cause the retries. A retry budget (--retry-budget 0.1) caps " +
				"retries as a share of traffic, so a broad outage stops being retried instead of being amplified."
		}
		out = append(out, finding)
	}

	if shed := s.Outcomes[metrics.OutcomeCircuitOpen]; shed > 0 {
		out = append(out, Finding{
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("the circuit breaker shed %s requests (%s)", Count(shed), Pct(s.Rate(metrics.OutcomeCircuitOpen))),
			Detail: "Those requests failed instantly instead of waiting for a timeout. They are still failures, " +
				"but a caller that fails in microseconds is not holding connections and goroutines open — which " +
				"is what stops one sick dependency from taking its callers down with it.",
		})
	}

	return out
}

// medianByQuarter compares the first and last quarter of the run.
func medianByQuarter(s *metrics.Summary) (early, late time.Duration, ok bool) {
	buckets := s.Buckets
	if len(buckets) < 8 {
		return 0, 0, false
	}
	quarter := len(buckets) / 4
	first := buckets[:quarter]
	last := buckets[len(buckets)-quarter:]

	early = medianOfBuckets(first)
	late = medianOfBuckets(last)
	if early <= 0 || late <= 0 {
		return 0, 0, false
	}
	return early, late, true
}

func medianOfBuckets(buckets []metrics.Bucket) time.Duration {
	var total time.Duration
	var count int
	for _, b := range buckets {
		if b.Latency.Count == 0 {
			continue
		}
		total += b.Latency.P50
		count++
	}
	if count == 0 {
		return 0
	}
	return total / time.Duration(count)
}

// failureRateByHalf compares the failure rate of each half of the run.
func failureRateByHalf(s *metrics.Summary) (early, late float64, ok bool) {
	if len(s.Buckets) < 4 {
		return 0, 0, false
	}
	mid := len(s.Buckets) / 2
	early, ok1 := failureRate(s.Buckets[:mid])
	late, ok2 := failureRate(s.Buckets[mid:])
	return early, late, ok1 && ok2
}

func failureRate(buckets []metrics.Bucket) (float64, bool) {
	var total, failed int
	for _, b := range buckets {
		total += b.Count
		failed += b.Failed
	}
	if total == 0 {
		return 0, false
	}
	return float64(failed) / float64(total), true
}
