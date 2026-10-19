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

> Status: early. The load driver and fault-injecting mock server work today.
> Retries, circuit breakers, and the fault-injecting proxy are next — see
> [ROADMAP.md](ROADMAP.md).

## Install

Requires Go 1.24+.

```bash
git clone https://github.com/saikiransangam/shakedown
cd shakedown
go build -o shakedown ./cmd/shakedown
```

## Quickstart

Start the built-in unreliable API in one terminal:

```bash
./shakedown mock --profile flaky
```

Point the driver at it in another:

```bash
./shakedown run --target http://127.0.0.1:9090/orders -c 50 -d 10s --timeout 2s
```

```
shakedown http://127.0.0.1:9090/orders
────────────────────────────────────────────────────────────────────────
  duration      9.99s                  concurrency   50
  requests      5,423                  throughput    543.0 req/s
  failures      690 (12.7%)            rate limit    unlimited
  transferred   375.1 KB               conn reuse    95.7%

Outcomes
  success            4,733   87.3%  █████████████████████
  server_error         561   10.3%  ██
  timeout              124    2.3%  █
  conn_reset             5    0.1%

Latency (successful requests)
  min 10ms   p50 41ms   p90 65ms   p95 67ms   p99 70ms   max 71ms

Where the time went (median per phase)
  dns —   connect 237us   tls —   ttfb 41ms   body 29us

Over time (per second, 10s total)
  req/s      █▄▅▅▄▅▅▅▄▄
  p99        ▇▇▇▇▇▇█▇▁▁
  failures   █▃▅▅▄▄▄▅▃▃

Diagnosis
  ! 2.3% of requests timed out
    The server accepted the request and never answered in time. That
    is usually a dependency that has no timeout of its own, a lock or
    connection pool that requests are queued behind, or simply more
    load than it has capacity for.
  ...
```

### Fault profiles

```bash
./shakedown mock --list-profiles
```

| profile    | what it does |
|------------|--------------|
| `healthy`  | fast and boring — a baseline to sanity-check the tool itself |
| `slow`     | always succeeds, always slow |
| `flaky`    | mostly fine, with 503s, hangs and dropped TCP connections mixed in |
| `brownout` | fast when idle; latency climbs with load, then it sheds requests |
| `tarpit`   | accepts every connection and never answers |

`brownout` is the interesting one. Run it at `-c 10` and it answers in ~20ms.
Run it at `-c 80` and p50 becomes ~580ms — the work did not get slower, requests
are just queueing behind a server that can only do ten things at once. That is
what most "the API is slow" incidents actually are.

Note that `/healthz` always returns a fast 200 regardless of profile, which is
exactly how a real service hides a brownout from its load balancer.

## What the output means

**Outcomes** — the error taxonomy. "8% errors" is not actionable; these are:

| outcome | what it tells you |
|---------|-------------------|
| `timeout` | the server took the request and never answered in time |
| `conn_reset` | the connection died mid-request — restart, proxy idle timeout, or a connection limit |
| `conn_refused` | nothing accepted the connection — process down, or the kernel accept queue overflowed |
| `server_error` | 5xx: the server knew it was failing |
| `client_error` | 4xx: usually the request is wrong, so the run is not measuring the happy path |
| `dns_failure` / `tls_failure` | the request never got as far as the application |

**Where the time went** — a request is not one latency number. If TTFB
dominates, the server is thinking and you should profile the handler. If connect
dominates, the problem is the path to it or its accept queue. A dash means the
phase did not happen: reused keep-alive connections skip DNS, connect and TLS
entirely, which is why `conn reuse` is reported too.

**Percentiles** — exact, not bucketed. Every sample is kept (8 bytes each), so a
million requests costs 8MB and the numbers are real measurements rather than
histogram approximations. The `tail ratio` is p99/p50; under ~5 is healthy, past
~10 something is queueing.

**Over time** — per-second buckets. A service that degrades over a run (leaks,
unbounded queues, a cache that stops fitting) looks completely different from
one that is uniformly bad, and a single aggregate number hides the difference.

## Flags

```
shakedown run
  --target URL            what to test (required)
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
  --max-failure-rate R    exit non-zero above this failure rate (for CI)
```

## Why unthrottled runs can mislead you

With `--rate` unset, workers send as fast as the target will accept. That
measures capacity, but it also means a target that fails *fast* (a load shedder
returning instant 503s) gets hammered far harder than a healthy one — you can
record a six-figure request rate made entirely of rejections. When you want to
know how a service behaves at a *specific* load, set `--rate`.

## Development

```bash
make test     # go test ./...
make check    # fmt, vet, test
make build    # ./shakedown
```

## License

MIT — see [LICENSE](LICENSE).
