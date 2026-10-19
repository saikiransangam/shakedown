package report

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/saikiransangam/shakedown/ramp"
)

// Ramp writes the report for a concurrency sweep.
func Ramp(w io.Writer, result *ramp.Result, analysis ramp.Analysis) {
	st := style{}
	if f, ok := w.(*os.File); ok {
		st = newStyle(f)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s %s\n", st.bold("shakedown ramp"), st.dim(result.Target))
	fmt.Fprintln(w, st.dim(strings.Repeat("─", lineWidth)))

	if len(result.Steps) == 0 {
		fmt.Fprintln(w, "  no steps completed")
		return
	}

	writeRampTable(w, st, result, analysis)
	writeRampCurves(w, st, result)
	writeCapacity(w, st, analysis)
	writeFindings(w, st, diagnoseRamp(result, analysis))
}

func writeRampTable(w io.Writer, st style, result *ramp.Result, analysis ramp.Analysis) {
	fmt.Fprintf(w, "  %s\n", st.dim(fmt.Sprintf("%6s %10s %9s %9s %-8s %9s",
		"conc", "req/s", "p50", "p99", "  fail", "vs base")))

	for _, step := range result.Steps {
		s := step.Summary
		inflation := "—"
		if analysis.BaseLatency > 0 && s.Success.P50 > 0 {
			inflation = fmt.Sprintf("%.1fx", float64(s.Success.P50)/float64(analysis.BaseLatency))
		}

		failText := Pct(s.FailureRate())
		if s.FailureRate() > 0.01 {
			failText = st.red(failText)
		}

		line := fmt.Sprintf("%6d %10.1f %9s %9s %s %9s",
			step.Concurrency,
			s.Throughput(),
			Dur(s.Success.P50),
			Dur(s.Success.P99),
			Pad("  "+failText, 8),
			inflation)

		// Mark the peak so the knee is findable without reading every row.
		if step.Concurrency == analysis.Peak.Concurrency {
			fmt.Fprintf(w, "  %s %s\n", line, st.green("← peak"))
			continue
		}
		fmt.Fprintf(w, "  %s\n", line)
	}

	if result.Interrupted {
		fmt.Fprintf(w, "  %s\n", st.amber("sweep was interrupted; later levels were not run"))
	}
}

func writeRampCurves(w io.Writer, st style, result *ramp.Result) {
	if len(result.Steps) < 3 {
		return
	}
	throughput := make([]float64, 0, len(result.Steps))
	tail := make([]float64, 0, len(result.Steps))
	for _, step := range result.Steps {
		throughput = append(throughput, step.Summary.Throughput())
		tail = append(tail, float64(step.Summary.Success.P99))
	}

	fmt.Fprintf(w, "\n%s %s\n", st.bold("Curves"), st.dim("(one cell per level, left to right)"))
	fmt.Fprintf(w, "  %-8s %s\n", "req/s", Spark(throughput))
	fmt.Fprintf(w, "  %-8s %s\n", "p99", Spark(tail))
}

func writeCapacity(w io.Writer, st style, a ramp.Analysis) {
	fmt.Fprintf(w, "\n%s\n", st.bold("Capacity"))
	fmt.Fprintf(w, "  %s %.1f req/s at %d concurrent\n",
		Pad(st.dim("peak throughput"), 20), a.Peak.Summary.Throughput(), a.Peak.Concurrency)
	fmt.Fprintf(w, "  %s %s unloaded → %s at the top level (%.1fx)\n",
		Pad(st.dim("median latency"), 20), Dur(a.BaseLatency), Dur(a.TopLatency), a.LatencyInflation)

	if a.SLO > 0 {
		if a.SLONeverMet {
			fmt.Fprintf(w, "  %s %s\n", Pad(st.dim("p99 budget"), 20),
				st.red(fmt.Sprintf("never met %s, even at the lowest level", Dur(a.SLO))))
		} else {
			fmt.Fprintf(w, "  %s met up to %d concurrent (%.1f req/s)\n",
				Pad(st.dim("p99 budget"), 20), a.SLOLimit, a.SLOThroughput)
		}
	}
}

func diagnoseRamp(result *ramp.Result, a ramp.Analysis) []Finding {
	var findings []Finding

	if !a.Saturated {
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("no limit found: throughput was still climbing at %d concurrent", a.Peak.Concurrency),
			Detail:   "The service never saturated in this sweep, so its real capacity is higher than anything tested. Raise --to and run again.",
		})
		return findings
	}

	findings = append(findings, Finding{
		Severity: SeverityInfo,
		Title:    fmt.Sprintf("capacity is about %.0f req/s, reached at %d concurrent", a.Peak.Summary.Throughput(), a.Peak.Concurrency),
		Detail: "Past this point the service does not do more work; requests just wait longer. This is the number to " +
			"size against, and the number a rate limit or a concurrency cap should be set near.",
	})

	if a.NegativeReturns {
		findings = append(findings, Finding{
			Severity: SeverityBad,
			Title: fmt.Sprintf("negative returns past %d concurrent: throughput fell %s below peak",
				a.Peak.Concurrency, Pct(a.ThroughputDrop)),
			Detail: "Adding load made the service complete less work in total, not more. Contention is now costing " +
				"more than the extra parallelism gains — lock contention, CPU thrash, or a connection pool being " +
				"fought over. Clients that retry into this will deepen it, which is how a slow service becomes an " +
				"outage.",
		})
	}

	if a.LatencyInflation >= 2 && a.QueueShare > 0 {
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title: fmt.Sprintf("at the top level, %s of each request is waiting rather than being served",
				Pct(a.QueueShare)),
			Detail: fmt.Sprintf("The work itself takes about %s — that is what it cost unloaded. The other %s is queueing. "+
				"Making the handler faster will barely help here; reducing concurrency or adding capacity will.",
				Dur(a.BaseLatency), Dur(a.TopLatency-a.BaseLatency)),
		})
	}

	if a.FirstFailureAt > 0 {
		findings = append(findings, Finding{
			Severity: SeverityBad,
			Title:    fmt.Sprintf("errors begin at %d concurrent", a.FirstFailureAt),
			Detail:   "Below this the service is slow but correct; at or above it, callers start seeing failures. That boundary is what an alert threshold should be derived from.",
		})
	} else {
		findings = append(findings, Finding{
			Severity: SeverityWarn,
			Title:    "the service never returned errors, it only got slower",
			Detail: "Every level succeeded, so an error-rate monitor would have called this healthy throughout — " +
				"including at the top level. Brownouts are invisible to error-rate alerting, which is why latency " +
				"objectives exist.",
		})
	}

	SortFindings(findings)
	return findings
}
