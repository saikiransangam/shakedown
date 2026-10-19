package report

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/saikiransangam/shakedown/metrics"
)

const lineWidth = 72

// Text writes a human-readable report for a finished run.
func Text(w io.Writer, s *metrics.Summary) {
	st := style{}
	if f, ok := w.(*os.File); ok {
		st = newStyle(f)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s %s\n", st.bold("shakedown"), st.dim(s.Target))
	fmt.Fprintln(w, st.dim(strings.Repeat("─", lineWidth)))

	writeOverview(w, st, s)
	writeOutcomes(w, st, s)
	writeStatuses(w, st, s)
	writeLatency(w, st, s)
	writePhases(w, st, s)
	writeTimeline(w, st, s)
	writeFindings(w, st, Diagnose(s))
}

func writeOverview(w io.Writer, st style, s *metrics.Summary) {
	rate := "unlimited"
	if s.RateLimit > 0 {
		rate = fmt.Sprintf("%.0f/s", s.RateLimit)
	}

	failureText := fmt.Sprintf("%s (%s)", Count(s.Failures()), Pct(s.FailureRate()))
	switch {
	case s.FailureRate() == 0:
		failureText = st.green(failureText)
	case s.FailureRate() > 0.01:
		failureText = st.red(failureText)
	default:
		failureText = st.amber(failureText)
	}

	row := func(leftLabel, leftValue, rightLabel, rightValue string) {
		fmt.Fprintf(w, "  %s %s %s %s\n",
			Pad(st.dim(leftLabel), 13), Pad(leftValue, 22),
			Pad(st.dim(rightLabel), 13), rightValue)
	}
	row("duration", Dur(s.Elapsed()), "concurrency", fmt.Sprintf("%d", s.Concurrency))
	row("requests", Count(s.Total), "throughput", fmt.Sprintf("%.1f req/s", s.Throughput()))
	row("failures", failureText, "rate limit", rate)
	row("transferred", Bytes(s.Bytes), "conn reuse", Pct(s.ReuseRate()))

	if s.Interrupted {
		fmt.Fprintf(w, "  %s\n", st.amber("run was interrupted; results cover the time completed"))
	}
}

func writeOutcomes(w io.Writer, st style, s *metrics.Summary) {
	fmt.Fprintf(w, "\n%s\n", st.bold("Outcomes"))
	for _, outcome := range metrics.AllOutcomes() {
		n := s.Outcomes[outcome]
		if n == 0 {
			continue
		}
		ratio := s.Rate(outcome)
		label := st.green(string(outcome))
		if !outcome.OK() {
			label = st.red(string(outcome))
		}
		fmt.Fprintf(w, "  %s %9s  %6s  %s\n", Pad(label, 14), Count(n), Pct(ratio), st.dim(Bar(ratio, 24)))
	}
}

func writeStatuses(w io.Writer, st style, s *metrics.Summary) {
	if len(s.Statuses) == 0 {
		return
	}
	codes := make([]int, 0, len(s.Statuses))
	for code := range s.Statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	var parts []string
	for _, code := range codes {
		parts = append(parts, fmt.Sprintf("%d: %s", code, Count(s.Statuses[code])))
	}
	fmt.Fprintf(w, "\n%s\n  %s\n", st.bold("Status codes"), strings.Join(parts, "   "))
}

func writeLatency(w io.Writer, st style, s *metrics.Summary) {
	d := s.Success
	caption := "successful requests"
	if d.Count == 0 {
		d = s.All
		caption = "all requests (none succeeded)"
	}
	if d.Count == 0 {
		return
	}

	fmt.Fprintf(w, "\n%s %s\n", st.bold("Latency"), st.dim("("+caption+")"))
	fmt.Fprintf(w, "  min %s   p50 %s   p90 %s   p95 %s   p99 %s   max %s\n",
		Dur(d.Min), Dur(d.P50), Dur(d.P90), Dur(d.P95), Dur(d.P99), Dur(d.Max))
	fmt.Fprintf(w, "  %s\n", st.dim(fmt.Sprintf("mean %s   stddev %s   p99.9 %s   tail ratio %.1fx",
		Dur(d.Mean), Dur(d.StdDev), Dur(d.P999), d.TailRatio())))

	// With many failures the success-only numbers describe a shrinking
	// minority of traffic, which can look reassuring while most callers are
	// having a terrible time.
	if s.FailureRate() > 0.1 && s.All.Count > 0 {
		fmt.Fprintf(w, "  %s\n", st.dim(fmt.Sprintf("including failures:  p50 %s   p99 %s   max %s",
			Dur(s.All.P50), Dur(s.All.P99), Dur(s.All.Max))))
	}
}

func writePhases(w io.Writer, st style, s *metrics.Summary) {
	type phase struct {
		name string
		dist metrics.Distribution
	}
	phases := []phase{
		{"dns", s.DNS},
		{"connect", s.Connect},
		{"tls", s.TLS},
		{"ttfb", s.TTFB},
		{"body", s.Body},
	}

	var parts []string
	for _, p := range phases {
		if p.dist.Count == 0 {
			parts = append(parts, fmt.Sprintf("%s —", p.name))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", p.name, Dur(p.dist.P50)))
	}

	fmt.Fprintf(w, "\n%s %s\n", st.bold("Where the time went"), st.dim("(median per phase)"))
	fmt.Fprintf(w, "  %s\n", strings.Join(parts, "   "))
	fmt.Fprintf(w, "  %s\n", st.dim("a dash means the phase did not happen — reused connections skip dns/connect/tls"))
}

func writeTimeline(w io.Writer, st style, s *metrics.Summary) {
	if len(s.Buckets) < 3 {
		return
	}

	throughput := make([]float64, 0, len(s.Buckets))
	tail := make([]float64, 0, len(s.Buckets))
	failures := make([]float64, 0, len(s.Buckets))
	for _, b := range s.Buckets {
		throughput = append(throughput, float64(b.Count))
		tail = append(tail, float64(b.Latency.P99))
		failures = append(failures, float64(b.Failed))
	}

	fmt.Fprintf(w, "\n%s %s\n", st.bold("Over time"), st.dim(fmt.Sprintf("(per second, %ds total)", len(s.Buckets))))
	fmt.Fprintf(w, "  %-10s %s\n", "req/s", Spark(throughput))
	fmt.Fprintf(w, "  %-10s %s\n", "p99", Spark(tail))
	if s.Failures() > 0 {
		fmt.Fprintf(w, "  %-10s %s\n", "failures", st.red(Spark(failures)))
	}
}

func writeFindings(w io.Writer, st style, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	sort.SliceStable(findings, func(i, j int) bool {
		return findings[i].Severity > findings[j].Severity
	})

	fmt.Fprintf(w, "\n%s\n", st.bold("Diagnosis"))
	for _, f := range findings {
		marker := st.dim("·")
		switch f.Severity {
		case SeverityBad:
			marker = st.red("!")
		case SeverityWarn:
			marker = st.amber("!")
		}
		fmt.Fprintf(w, "  %s %s\n", marker, f.Title)
		for _, line := range wrap(f.Detail, lineWidth-6) {
			fmt.Fprintf(w, "    %s\n", st.dim(line))
		}
	}
	fmt.Fprintln(w)
}

func wrap(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	return append(lines, line)
}
