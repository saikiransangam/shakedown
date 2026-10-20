package report

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

// builder assembles a Summary for a diagnosis test. The rules are pure
// functions over this struct, so none of this needs a server.
type builder struct {
	s *metrics.Summary
}

func newSummary(total int) *builder {
	start := time.Now().Add(-10 * time.Second)
	return &builder{s: &metrics.Summary{
		Target:      "http://example.test/orders",
		Concurrency: 10,
		Start:       start,
		End:         start.Add(10 * time.Second),
		Total:       total,
		Attempts:    total,
		Outcomes:    map[metrics.Outcome]int{metrics.OutcomeSuccess: total},
		Statuses:    map[int]int{200: total},
		Success:     metrics.Distribution{Count: total, P50: 20 * time.Millisecond, P99: 30 * time.Millisecond},
	}}
}

// outcome moves n requests from success into the given outcome.
func (b *builder) outcome(o metrics.Outcome, n int) *builder {
	b.s.Outcomes[metrics.OutcomeSuccess] -= n
	b.s.Outcomes[o] += n
	b.s.Success.Count = b.s.Outcomes[metrics.OutcomeSuccess]
	return b
}

func (b *builder) latency(p50, p99 time.Duration) *builder {
	b.s.Success.P50 = p50
	b.s.Success.P99 = p99
	return b
}

func (b *builder) attempts(n int) *builder {
	b.s.Attempts = n
	return b
}

func (b *builder) phase(which string, d time.Duration, count int) *builder {
	dist := metrics.Distribution{Count: count, P50: d, P99: d}
	switch which {
	case "ttfb":
		b.s.TTFB = dist
	case "connect":
		b.s.Connect = dist
	}
	return b
}

func (b *builder) reuse(n int) *builder {
	b.s.Reused = n
	return b
}

// buckets lays out per-second buckets with the given medians, which is what
// the degradation rules read.
func (b *builder) buckets(medians []time.Duration, failedPerBucket int) *builder {
	perBucket := b.s.Total / len(medians)
	for i, median := range medians {
		b.s.Buckets = append(b.s.Buckets, metrics.Bucket{
			Second:  i,
			Count:   perBucket,
			Failed:  failedPerBucket,
			Latency: metrics.Distribution{Count: perBucket, P50: median, P99: median},
		})
	}
	return b
}

func (b *builder) build() *metrics.Summary { return b.s }

// titles returns every finding title, for substring assertions.
func titles(findings []Finding) string {
	var sb strings.Builder
	for _, f := range findings {
		sb.WriteString(f.Title)
		sb.WriteString("\n")
	}
	return sb.String()
}

func hasFinding(t *testing.T, findings []Finding, substr string, want Severity) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Title, substr) {
			if f.Severity != want {
				t.Errorf("finding %q has severity %d, want %d", substr, f.Severity, want)
			}
			if f.Detail == "" {
				t.Errorf("finding %q has no explanation; the explanation is the point", substr)
			}
			return
		}
	}
	t.Errorf("expected a finding mentioning %q, got:\n%s", substr, titles(findings))
}

func hasNoFinding(t *testing.T, findings []Finding, substr string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Title, substr) {
			t.Errorf("did not expect a finding mentioning %q, got:\n%s", substr, titles(findings))
		}
	}
}

func TestDiagnoseEmptyRun(t *testing.T) {
	findings := Diagnose(&metrics.Summary{Outcomes: map[metrics.Outcome]int{}})

	if len(findings) != 1 {
		t.Fatalf("want exactly one finding for an empty run, got %d", len(findings))
	}
	hasFinding(t, findings, "no requests completed", SeverityBad)
}

func TestDiagnoseHealthyRunSaysSo(t *testing.T) {
	findings := Diagnose(newSummary(1000).build())

	if len(findings) != 1 {
		t.Fatalf("a clean run should produce one reassuring finding, got:\n%s", titles(findings))
	}
	hasFinding(t, findings, "no reliability problems detected", SeverityInfo)
}

func TestDiagnoseConnectivityFailures(t *testing.T) {
	cases := []struct {
		outcome metrics.Outcome
		substr  string
	}{
		{metrics.OutcomeConnRefused, "refused outright"},
		{metrics.OutcomeConnReset, "died mid-request"},
		{metrics.OutcomeDNSFailure, "DNS lookups failed"},
		{metrics.OutcomeTLSFailure, "TLS handshakes failed"},
	}
	for _, c := range cases {
		// Even a single occurrence matters: these never happen on a healthy
		// path, so there is no noise floor to filter out.
		findings := Diagnose(newSummary(1000).outcome(c.outcome, 1).build())
		hasFinding(t, findings, c.substr, SeverityBad)
	}
}

func TestDiagnoseTimeoutThreshold(t *testing.T) {
	// 0.5% is below the 1% threshold and should stay quiet.
	quiet := Diagnose(newSummary(1000).outcome(metrics.OutcomeTimeout, 5).build())
	hasNoFinding(t, quiet, "timed out")

	loud := Diagnose(newSummary(1000).outcome(metrics.OutcomeTimeout, 50).build())
	hasFinding(t, loud, "timed out", SeverityBad)
}

func TestDiagnoseServerErrorThreshold(t *testing.T) {
	quiet := Diagnose(newSummary(1000).outcome(metrics.OutcomeServerError, 5).build())
	hasNoFinding(t, quiet, "returned 5xx")

	loud := Diagnose(newSummary(1000).outcome(metrics.OutcomeServerError, 100).build())
	hasFinding(t, loud, "returned 5xx", SeverityBad)
}

// Failures that climb during a run point at exhaustion rather than a bug on
// every request, and the explanation should say which one it is.
func TestDiagnoseServerErrorsThatGrowOverTheRun(t *testing.T) {
	b := newSummary(800).outcome(metrics.OutcomeServerError, 200)
	// Eight buckets of 100: clean first half, failing second half.
	for i := range 8 {
		failed := 0
		if i >= 4 {
			failed = 50
		}
		b.s.Buckets = append(b.s.Buckets, metrics.Bucket{
			Second:  i,
			Count:   100,
			Failed:  failed,
			Latency: metrics.Distribution{Count: 100, P50: 20 * time.Millisecond},
		})
	}

	findings := Diagnose(b.build())

	for _, f := range findings {
		if strings.Contains(f.Title, "returned 5xx") {
			if !strings.Contains(f.Detail, "first half") {
				t.Errorf("expected the detail to report the growth, got: %s", f.Detail)
			}
			return
		}
	}
	t.Fatalf("no 5xx finding at all:\n%s", titles(findings))
}

func TestDiagnoseMostlyClientErrorsWarnsTheRunIsInvalid(t *testing.T) {
	findings := Diagnose(newSummary(1000).outcome(metrics.OutcomeClientError, 900).build())
	hasFinding(t, findings, "returned 4xx", SeverityWarn)
}

func TestDiagnoseLongTail(t *testing.T) {
	// p99 20x p50 is a queueing signature.
	findings := Diagnose(newSummary(1000).
		latency(20*time.Millisecond, 400*time.Millisecond).build())
	hasFinding(t, findings, "long tail", SeverityWarn)

	// A healthy spread must not trip it.
	hasNoFinding(t, Diagnose(newSummary(1000).
		latency(20*time.Millisecond, 60*time.Millisecond).build()), "long tail")
}

// With too few samples a percentile is not evidence of anything.
func TestDiagnoseIgnoresLongTailOnTinySamples(t *testing.T) {
	findings := Diagnose(newSummary(10).
		latency(10*time.Millisecond, 5*time.Second).build())
	hasNoFinding(t, findings, "long tail")
}

func TestDiagnoseLatencyDegradationOverTime(t *testing.T) {
	// Eight buckets climbing from 20ms to 300ms.
	findings := Diagnose(newSummary(800).buckets([]time.Duration{
		20 * time.Millisecond, 22 * time.Millisecond,
		40 * time.Millisecond, 80 * time.Millisecond,
		120 * time.Millisecond, 180 * time.Millisecond,
		250 * time.Millisecond, 300 * time.Millisecond,
	}, 0).build())

	hasFinding(t, findings, "degraded during the run", SeverityWarn)
}

func TestDiagnoseStableLatencyIsNotReportedAsDrift(t *testing.T) {
	findings := Diagnose(newSummary(800).buckets([]time.Duration{
		20 * time.Millisecond, 21 * time.Millisecond, 20 * time.Millisecond, 22 * time.Millisecond,
		21 * time.Millisecond, 20 * time.Millisecond, 23 * time.Millisecond, 21 * time.Millisecond,
	}, 0).build())

	hasNoFinding(t, findings, "degraded during the run")
}

func TestDiagnoseServerBoundRequests(t *testing.T) {
	// 180ms of a 200ms request is time-to-first-byte: the handler is slow,
	// not the network.
	findings := Diagnose(newSummary(1000).
		latency(200*time.Millisecond, 220*time.Millisecond).
		phase("ttfb", 180*time.Millisecond, 1000).build())

	hasFinding(t, findings, "waiting for the first byte", SeverityInfo)
}

// A fast request that is mostly TTFB is normal and not worth reporting.
func TestDiagnoseIgnoresTTFBShareOnFastRequests(t *testing.T) {
	findings := Diagnose(newSummary(1000).
		latency(10*time.Millisecond, 12*time.Millisecond).
		phase("ttfb", 9*time.Millisecond, 1000).build())

	hasNoFinding(t, findings, "waiting for the first byte")
}

func TestDiagnoseSlowConnectAndLowReuse(t *testing.T) {
	findings := Diagnose(newSummary(1000).
		phase("connect", 120*time.Millisecond, 1000).
		reuse(100). // 10% reuse
		build())

	hasFinding(t, findings, "connection setup is slow", SeverityWarn)
	hasFinding(t, findings, "reused a connection", SeverityWarn)
}

func TestDiagnoseRetryAmplification(t *testing.T) {
	// Retries helping a mostly-healthy target: worth noting, not alarming.
	findings := Diagnose(newSummary(1000).
		outcome(metrics.OutcomeServerError, 50).
		attempts(1500).build())
	hasFinding(t, findings, "amplified load", SeverityWarn)

	// A small number of retries is not worth a finding at all.
	hasNoFinding(t, Diagnose(newSummary(1000).attempts(1050).build()), "amplified load")
}

func TestDiagnoseRetryStorm(t *testing.T) {
	// The same amplification against a target failing most requests is the
	// self-sustaining case, and must be escalated.
	findings := Diagnose(newSummary(1000).
		outcome(metrics.OutcomeServerError, 900).
		attempts(2800).build())

	hasFinding(t, findings, "retry storm", SeverityBad)
	for _, f := range findings {
		if strings.Contains(f.Title, "retry storm") && !strings.Contains(f.Detail, "retry-budget") {
			t.Error("the storm finding should name the flag that fixes it")
		}
	}
}

func TestDiagnoseShedRequests(t *testing.T) {
	findings := Diagnose(newSummary(1000).
		outcome(metrics.OutcomeCircuitOpen, 300).build())

	hasFinding(t, findings, "circuit breaker shed", SeverityInfo)
}

// The most serious thing found should be the first thing read.
func TestDiagnoseOrdersFindingsBySeverity(t *testing.T) {
	findings := Diagnose(newSummary(1000).
		outcome(metrics.OutcomeConnReset, 20).   // bad
		outcome(metrics.OutcomeCircuitOpen, 50). // info
		latency(20*time.Millisecond, 900*time.Millisecond).
		build())

	assertSortedBySeverity(t, findings)
}

// A severe finding from a late rule must still sort ahead of a mild one from
// an early rule. The TTFB note comes from diagnosePhases and the storm from
// diagnosePolicies, which runs after it.
func TestDiagnoseOrderingDoesNotDependOnRuleOrder(t *testing.T) {
	findings := Diagnose(newSummary(1000).
		latency(200*time.Millisecond, 240*time.Millisecond).
		phase("ttfb", 190*time.Millisecond, 1000). // info, from an early rule
		outcome(metrics.OutcomeServerError, 900).
		attempts(2800). // bad, from a late rule
		build())

	hasFinding(t, findings, "waiting for the first byte", SeverityInfo)
	hasFinding(t, findings, "retry storm", SeverityBad)
	assertSortedBySeverity(t, findings)
}

func assertSortedBySeverity(t *testing.T, findings []Finding) {
	t.Helper()
	if len(findings) < 3 {
		t.Fatalf("expected several findings, got:\n%s", titles(findings))
	}
	severities := make([]Severity, len(findings))
	for i, f := range findings {
		severities[i] = f.Severity
	}
	if !slices.IsSortedFunc(severities, func(a, b Severity) int { return int(b) - int(a) }) {
		t.Errorf("findings are not ordered most-severe-first: %v\n%s", severities, titles(findings))
	}
}
