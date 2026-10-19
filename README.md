# shakedown

**Find out *why* an API is unreliable, not just that it is.**

Most load testers answer "how many requests per second can it take?". That is
rarely the question you actually have. The questions you have are:

- it times out sometimes — is that the network, the server, or my client?
- p99 is awful but p50 is fine — what is the tail waiting on?
- it falls over at some load — at what load, and what breaks first?
- my retries make it *worse* — why?

`shakedown` is built to answer those. Every request is classified by *how* it
failed (timeout vs. connection reset vs. 503 vs. refused), split into the phases
it spent time in (DNS, connect, TLS, time-to-first-byte, body), bucketed per
second so you can see degradation over time — and then the tool tells you what
the pattern usually means.

It also ships a deliberately broken API so you have something to test against
before you point it at anything real.

**Contents**

- [Install](#install)
- [Quickstart](#quickstart)
- [Commands](#commands)
- [Architecture, end to end](#architecture-end-to-end)
- [Design decisions, and the problem each one avoids](#design-decisions-and-the-problem-each-one-avoids)
- [Reading the output](#reading-the-output)
- [Status](#status)

## Install

Requires Go 1.24+. No third-party dependencies.

```bash
git clone https://github.com/saikiransangam/shakedown
cd shakedown
make build
```

## Quickstart

Start the built-in unreliable API in one terminal:

```bash
./shakedown mock --profile brownout
```

Find out where it stops coping, in another:

```bash
./shakedown ramp --target http://127.0.0.1:9090/orders --levels 1,5,10,20,40,80 --slo 100ms
```

```
    conc      req/s       p50       p99     fail   vs base
       1       47.1      21ms      31ms    0.0%      1.0x
       5      241.0      21ms      31ms    0.0%      1.0x
      10      487.3      20ms      30ms    0.0%      1.0x ← peak
      20      200.4     100ms     110ms    0.0%      4.7x
      40      154.9     258ms     271ms    0.0%     12.2x
      80      139.3     578ms     590ms    0.0%     27.3x

Curves (one cell per level, left to right)
  req/s    ▁▄█▃▃▃
  p99      ▁▁▁▂▄█

Capacity
  peak throughput      487.3 req/s at 10 concurrent
  median latency       21ms unloaded → 578ms at the top level (27.3x)
  p99 budget           met up to 10 concurrent (487.3 req/s)

Diagnosis
  ! negative returns past 10 concurrent: throughput fell 71.4% below peak
    Adding load made the service complete less work in total, not more...
  ! at the top level, 96.3% of each request is waiting rather than being served
    The work itself takes about 21ms — that is what it cost unloaded. The
    other 557ms is queueing...
  ! the service never returned errors, it only got slower
    Every level succeeded, so an error-rate monitor would have called this
    healthy throughout — including at the top level...
```

That `brownout` profile is configured to handle 10 concurrent requests. The
sweep found `10` without being told — that is the whole idea.

## Commands

### `run` — measure one load level

```
shakedown run --target URL [flags]

  -c N                    concurrent workers (default 10)
  -d DURATION             how long to run (default 10s)
  -n N                    stop after N requests instead
  --rate N                cap total requests/sec (0 = unthrottled)
  --timeout DURATION      per-request timeout (default 5s)
  --method METHOD         HTTP method
  -H 'Name: value'        request header (repeatable)
  --body STRING           request body
  --insecure              skip TLS verification
  --no-keepalive          fresh connection per request
  --follow-redirects      follow 3xx
  --max-failure-rate R    exit non-zero above this failure rate (for CI)
  --quiet                 no live progress
```

### `ramp` — find the capacity limit

One run tells you how a service behaves at one concurrency level. A *limit* is
only visible by comparing levels, so `ramp` sweeps them and finds the knee of
the throughput curve.

```
shakedown ramp --target URL [flags]

  --from N                lowest concurrency (default 1)
  --to N                  highest concurrency (default 64)
  --levels 1,10,50        explicit levels instead of --from/--to
  --step-duration DUR     measurement window per level (default 5s)
  --warmup DUR            unrecorded load before each level (default 1s)
  --slo DURATION          p99 budget; reports the highest concurrency meeting it
  --timeout DURATION      per-request timeout (default 5s)
```

Levels double rather than step linearly (`1,2,4,8,...`), because saturation
points span orders of magnitude — a linear sweep either misses a low limit or
wastes most of its steps above a high one.

### `mock` — be the unreliable service

```
shakedown mock [flags]

  --listen ADDR           default 127.0.0.1:9090
  --profile NAME          default flaky
  --list-profiles
```

| profile    | what it does |
|------------|--------------|
| `healthy`  | fast and boring — a baseline to sanity-check the tool itself |
| `slow`     | always succeeds, always slow |
| `flaky`    | mostly fine, with 503s, hangs and dropped TCP connections mixed in |
| `brownout` | fast when idle; latency climbs with load, then it sheds requests |
| `tarpit`   | accepts every connection and never answers |

`/healthz` always returns a fast 200 regardless of profile — exactly how a real
service hides a brownout from its load balancer.

## Architecture, end to end

### The shape of it

Four stages, each one a package that does not know how the next one works:
**generate load → measure one request → aggregate → interpret.**

```
  shakedown run                      shakedown ramp
        │                                   │
        │                                   │  for each concurrency level:
        │                                   │  build a probe, warm up, measure
        │                                   ▼
        │                            ┌──────────────┐
        └───────────────────────────▶│   load.Run   │◀──────── ramp.Run calls
                                     └──────┬───────┘          this per level
                                            │
                     ┌──────────────────────┴───────────────────────┐
                     │  N worker goroutines, all running this loop:  │
                     │    1. is the run over?        (context)       │
                     │    2. wait for a pacing tick  (rate limit)    │
                     │    3. res := probe.Do(ctx)                    │
                     │    4. send res on the results channel         │
                     └──────────────────────┬───────────────────────┘
                                            │ calls, once per request
                                            ▼
                                   ┌──────────────────┐
                                   │   probe.HTTP     │  one request:
                                   │   (load.Probe)   │  build, send, trace,
                                   └────────┬─────────┘  drain, classify
                                            │
                                            │  metrics.Result
                                            ▼
                               ╔════════════════════════╗
                               ║   results chan (buf)   ║   the only place
                               ╚════════════┬═══════════╝   goroutines meet
                                            │
                                            ▼
                                 ┌──────────────────────┐
                                 │  metrics.Collector   │  ONE goroutine owns
                                 │  taxonomy, samples,  │  all of this state,
                                 │  per-second buckets  │  so no mutexes
                                 └──────────┬───────────┘
                                            │  *metrics.Summary
                                            ▼
                          ┌─────────────────────────────────────┐
                          │  report.Diagnose / ramp.Analyze     │  pattern → cause
                          │  report.Text    / report.Ramp       │  render
                          └─────────────────────────────────────┘
```

### Package map

| package | owns | deliberately does not know |
|---------|------|----------------------------|
| `cmd/shakedown` | flag dispatch, signal handling, exit codes | anything about measurement |
| `internal/cli` | parsing flags into configs, wiring packages together | how any of them work |
| `load` | the concurrency engine: workers, pacing, cancellation | HTTP — it only sees `Probe` |
| `probe` | issuing one HTTP request and timing its phases | concurrency, aggregation |
| `metrics` | the failure taxonomy, percentiles, per-second buckets | where results came from |
| `ramp` | sweeping concurrency levels, finding the knee | HTTP, rendering |
| `internal/report` | diagnosis rules and terminal rendering | measurement |
| `mockapi` | being a broken service on purpose | the client side entirely |

The dependency arrows only ever point one way: `cli → {load, probe, ramp,
report} → metrics`. Nothing in `metrics` imports anything of ours, which is why
it is the package with real unit tests.

### The life of one request

What happens inside `probe.HTTP.Do`, in order:

1. **A deadline is attached to the context.** Not `http.Client.Timeout` — see
   [the design notes](#2-the-timeout-lives-on-the-context-not-on-httpclient).
2. **An `httptrace.ClientTrace` is attached**, which gives callbacks when DNS
   starts and ends, when the TCP connection is established, when the TLS
   handshake completes, when a connection is taken from the pool (and whether
   it was reused), and when the first response byte arrives.
3. **`start := time.Now()`**, then `client.Do(req)`.
4. If that returns an error, it goes to `metrics.Classify(0, err)` — the error
   never got a status code, so the *kind* of error is all the information there
   is, and extracting it is the single most valuable thing this tool does.
5. On success, **the body is read to completion and discarded**, then closed.
   Draining is what returns the connection to the keep-alive pool.
6. A read error mid-body **overrides a 200** — a response whose body died
   halfway is not a success, however the status line read.
7. The trace timestamps become a `metrics.Phases`: `DNS`, `Connect`, `TLS`,
   `TTFB` (start → first byte) and `Body` (first byte → done).
8. All of it returns as one `metrics.Result` value — no pointers, no shared
   state, nothing for another goroutine to race on.

### The concurrency model

This is the part worth understanding, since it is the reason the code has
almost no locks.

**Workers share nothing.** Each of the N worker goroutines builds its own
request and produces its own `Result` value. The only thing they share is the
`http.Transport`'s connection pool, which is already safe for concurrent use.

**Results travel by channel, not by shared memory.** Every worker sends its
`Result` into one buffered channel. That channel is the only point where
goroutines interact.

**One goroutine owns all aggregated state.** `metrics.Collector.Collect` ranges
over the channel until it closes. Because exactly one goroutine ever touches
the maps and sample slices, they need no mutex at all. This is the Go proverb
in practice: *don't communicate by sharing memory; share memory by
communicating.*

**Atomics appear exactly twice**, for the live progress counters (`sent`,
`failed`). There is one writer (the collector) and one reader (the progress
ticker), and the reader only needs a roughly-current number — so an atomic is
the right tool and a mutex would be overkill.

**Shutdown is a context tree**, not a kill switch:

```
signal.NotifyContext            ← Ctrl-C / SIGTERM lands here
  └─ runCtx (cancel on duration elapsed)
       └─ per-request context.WithTimeout(ctx, --timeout)
```

A cancel at any level propagates down, so Ctrl-C aborts in-flight requests
immediately — and then `load.Run` still returns a summary, so you get the
report for the part that did run rather than losing the whole thing.

The sequence at the end matters:

```go
wg.Wait()         // every worker has stopped sending
close(results)    // therefore closing is safe, and ends the collector's range
summary := <-done // the collector hands over the finished summary
```

Closing before `wg.Wait()` would panic on a send to a closed channel; reading
`done` before closing would deadlock. Both are the classic mistakes, and the
ordering above is the fix.

**`shakedown ramp` is a loop around all of this.** It calls `load.Run` once per
concurrency level with a fresh probe (the connection pool has to be sized to
each level), throws away a warmup run, keeps the measured one, and hands the
list of summaries to `ramp.Analyze`.

### How a diagnosis is produced

`metrics.Summary` is the handoff: counts per outcome, counts per status code,
exact latency distributions for all requests and for successful ones
separately, a distribution per request phase, and one bucket per second of the
run.

`report.Diagnose` and `ramp.Analyze` are then pure functions over that struct —
no I/O, no clock, no network. Each rule is a threshold plus an explanation of
what the pattern usually means:

| observation | what it is reported as |
|-------------|------------------------|
| p99 / p50 > 10 | a long tail: requests are waiting, not working |
| timeouts > 1% | the server took the request and never answered |
| any `conn_refused` | nothing accepted the connection — dead process, or a full accept queue |
| any `conn_reset` | connection died mid-request — restart, proxy idle timeout, connection limit |
| failure rate higher in the second half | something is being exhausted, not a bug on every request |
| median p50 in the last quarter > 2× the first | it degraded during the run, so these numbers are a floor |
| TTFB > 80% of the request | the server is thinking; profile the handler, not the network |
| connection reuse < 50% | mostly paying for handshakes, not measuring the service |
| throughput at the top level < 90% of peak | negative returns: more load, less work done |

Because they are pure functions over a plain struct, the thresholds are
testable without a server — which is what `ramp/ramp_test.go` does with
synthetic steps.

## Design decisions, and the problem each one avoids

Each of these is a line of code that looks arbitrary until you know what goes
wrong without it.

#### 1. The connection pool is sized to the run's concurrency

Go's `http.Transport` defaults to `MaxIdleConnsPerHost: 2`. Run 50 workers
against that and 48 of them are opening and closing TCP connections instead of
reusing them. You measure handshakes, you report latency that belongs to your
own load generator, and the target looks worse than it is. The pool is set to
the worker count for this reason.

#### 2. The timeout lives on the context, not on `http.Client`

`http.Client.Timeout` produces an error that is awkward to classify.
`context.WithTimeout` produces one that satisfies
`errors.Is(err, context.DeadlineExceeded)`, which is how `timeout` stays
distinct from `conn_reset` in the taxonomy. The whole value of the tool rests
on that distinction, so it is worth the extra line.

#### 3. The response body is drained before being closed

`io.Copy(io.Discard, resp.Body)` then `Close()`. Closing a partially-read body
makes net/http discard the connection, so the next request pays for a fresh
handshake. Skipping the drain quietly triples measured latency under load.

#### 4. Every latency sample is kept

8 bytes per request, so a million requests costs 8MB — cheap enough that there
is no reason to approximate. Percentiles are computed by sorting and taking the
nearest rank, so every number printed is a real measurement rather than a
histogram bucket boundary.

#### 5. Rate limiting is one shared ticker, and dropped ticks are the point

All workers receive from a single `time.Ticker`; a tick goes to exactly one of
them, which makes it a global limit rather than a per-worker one. When every
worker is busy, ticks are *dropped* rather than queued. That matters: a token
bucket that accrues credit while the target is stalled will fire its entire
backlog the instant it recovers — which is precisely when a struggling service
can least afford a burst.

#### 6. The runner never mentions HTTP

`load.Run` takes a `Probe` interface. That is why the concurrency engine can be
tested with a synthetic probe and no network at all, and why a gRPC or database
probe can be added later without touching the engine.

#### 7. Cancellations we caused are discarded, not counted

When the duration elapses or you press Ctrl-C, in-flight requests fail with
`context.Canceled`. Recording those would mean every run ended with a burst of
fake failures — and worse, pressing Ctrl-C would *change the verdict*. Workers
drop results whose outcome is `canceled` when the run context is already done.

#### 8. Zero-valued phases are skipped when aggregating

A reused keep-alive connection does no DNS lookup and no handshake, so those
phases are legitimately zero. Averaging those zeros in would drag the median
DNS time to nothing and hide a genuinely slow resolver. Only phases that
actually happened are aggregated, and the report prints a dash for the rest.

#### 9. Each ramp level gets a warmup that is thrown away

A fresh probe means a cold connection pool, so the first requests of every
level pay for handshakes. Charging that to the measurement would make low
concurrency levels look artificially slow, which is exactly where the baseline
comes from.

#### 10. Redirects are not followed by default

You asked to measure a specific endpoint. Following a 302 silently measures a
different one.

#### 11. No third-party dependencies

Standard library `flag` instead of a CLI framework, no metrics library, no
assertion library. It keeps the build instant and the supply chain empty, and
for a project whose purpose is learning how this machinery works, importing
something that already does it would defeat the point.

## Reading the output

**Outcomes** — the error taxonomy. "8% errors" is not actionable; these are:

| outcome | what it tells you |
|---------|-------------------|
| `timeout` | the server took the request and never answered in time |
| `conn_reset` | the connection died mid-request — restart, proxy idle timeout, or a connection limit |
| `conn_refused` | nothing accepted the connection — process down, or the kernel accept queue overflowed |
| `server_error` | 5xx: the server knew it was failing |
| `client_error` | 4xx: usually the request is wrong, so the run is not measuring the happy path |
| `dns_failure` / `tls_failure` | the request never got as far as the application |
| `canceled` | we gave up, not the server |

**Where the time went** — a request is not one latency number. If TTFB
dominates, the server is thinking and you should profile the handler. If
connect dominates, the problem is the path to it or its accept queue. A dash
means the phase did not happen.

**Tail ratio** (p99/p50) — under ~5 is healthy; past ~10 something is queueing.
A *long tail* means most requests are fine and a few are terrible. *Uniform*
slowness (tail ratio ≈ 1, but a high p50) means everyone is equally miserable
— that is saturation. Same symptom to a user, opposite fixes.

**Over time** — per-second buckets. A service that degrades across a run
(leaks, unbounded queues, a cache that stops fitting) looks nothing like one
that is uniformly bad, and a single aggregate number hides the difference.

### Little's Law, which you can check yourself

`concurrency ≈ throughput × latency`. From the sweep above:

- at 10 workers: `487 × 0.020s = 9.7` ✓
- at 80 workers: `139 × 0.578s = 80.4` ✓

Both land on the worker count. At 80 the service is not doing eight times the
work — those extra 70 workers are *sitting in a queue*. When this identity
holds while throughput is flat, every unit of concurrency you add becomes pure
waiting. That is the arithmetic behind the `brownout` profile, and behind most
real "the API is slow" incidents.

### Why unthrottled runs can mislead you

With `--rate` unset, workers send as fast as the target accepts. That measures
capacity, but it also means a target that fails *fast* — a load shedder
returning instant 503s — gets hammered far harder than a healthy one. You can
record a six-figure request rate made entirely of rejections. To ask how a
service behaves at a *specific* load, set `--rate`.

## Status

Early, and honest about it: the load driver, the concurrency sweep, and the
fault-injecting mock server work today. Retries, circuit breakers and the
fault-injecting proxy are next — see [ROADMAP.md](ROADMAP.md).

## Development

```bash
make build    # ./shakedown
make test     # go test -race ./...
make check    # fmt, vet, test
make demo     # start a flaky API, hit it, shut it down
```

## License

MIT — see [LICENSE](LICENSE).
