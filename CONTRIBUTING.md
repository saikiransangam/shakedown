# Contributing

Thanks for looking. This project is also a learning exercise, so explanations
are treated as part of the deliverable rather than a nice-to-have.

## Getting set up

Requires Go 1.24+. There are no third-party dependencies, and adding one needs
a good reason.

```bash
git clone https://github.com/saikiransangam/shakedown
cd shakedown
make check   # gofmt, vet, go test -race
make build
```

To try a change by hand, run the mock server in one terminal and point the
tool at it in another:

```bash
./shakedown mock --profile brownout
./shakedown ramp --target http://127.0.0.1:9090/orders --to 128 --slo 100ms
```

## What makes a good change here

The purpose of this tool is to explain *why* a service is unreliable. A change
that adds a number is less valuable than one that explains a number.

- **A new diagnosis rule** is the most welcome kind of change: a pattern in a
  `metrics.Summary` plus a plain-language account of what that pattern usually
  means. See `internal/report/diagnose.go`.
- **A new fault profile** in `mockapi` is a close second — a failure mode that
  real services have and this one cannot yet reproduce.
- **A new probe** (gRPC, TCP, a database) only has to satisfy `load.Probe`. The
  load engine deliberately knows nothing about HTTP.

## Expectations for a pull request

1. `make check` passes. CI runs `gofmt`, `go vet` and `go test -race`, plus a
   smoke test of the real binary against the mock server.
2. **Data races are correctness bugs.** This is a concurrency tool; `-race`
   failures are never flakes to be retried.
3. **Thresholds get tests.** Diagnosis rules are pure functions over a
   `metrics.Summary`, so they can be tested with a synthetic summary and no
   network — see `internal/report/diagnose_test.go`. A rule without a test
   that pins both sides of its threshold is a rule that will drift.
4. **Explain the non-obvious, not the obvious.** Comments here answer "why is
   this line here", because most of the subtle code exists to avoid a specific
   measurement error. `// increment the counter` is noise; `// draining the
   body is what returns the connection to the pool` is the reason the line
   cannot be deleted.

## Measurement integrity

Most of the awkward parts of this codebase exist because it is easy to build a
load tester that measures itself. If you touch the request path, these are the
traps, each of which has a note in [the README](README.md#design-decisions-and-the-problem-each-one-avoids):

- The connection pool must be sized to concurrency, or you measure TCP
  handshakes instead of the service.
- Response bodies must be drained before closing, or keep-alive stops working.
- Timeouts belong on the context, not on `http.Client`, or `timeout` becomes
  indistinguishable from `conn_reset`.
- Cancellations we caused must not be counted, or pressing Ctrl-C changes the
  verdict of a run.
- **Comparisons need the same arrival rate on both sides.** Without `--rate`,
  a change that alters how long requests take also alters how many get sent,
  and the comparison measures the load generator rather than the target.

## Reporting a bug

A useful report includes the exact command, what the report said, and what you
expected it to say. If it involves a real service, the `mock` profile that
comes closest to reproducing it is more useful than the service's name.
