# Roadmap

Ordered so that each step is usable on its own, and so that each one teaches the
concept it implements.

## Done — step 1: measure and classify

- [x] Concurrent load driver (worker pool, context cancellation, graceful Ctrl-C)
- [x] Global rate limiting
- [x] Error taxonomy — timeout / reset / refused / 5xx / DNS / TLS, not just "error"
- [x] Per-phase timing via `httptrace` (DNS, connect, TLS, TTFB, body)
- [x] Exact percentiles, per-second buckets, degradation-over-time detection
- [x] Diagnosis rules that say what a pattern usually means
- [x] Fault-injecting mock API with profiles, including TCP resets and saturation

## Done — step 2: ramp, find the knee

A single run at one concurrency level tells you how the service behaves *there*.
The useful question is where it stops coping.

- [x] `shakedown ramp --target URL --from 1 --to 200` — sweep concurrency
      levels, report throughput, latency and error rate per step
- [x] Geometric (doubling) levels, or an explicit `--levels` list
- [x] Per-level warmup so a cold connection pool is not charged to the baseline
- [x] Detect the knee: the concurrency at peak throughput is the real capacity
- [x] Detect negative returns — throughput *falling* as load rises
- [x] Report what share of a request is queueing rather than being served
- [x] `--slo` to report the highest concurrency meeting a p99 budget
- [ ] Open-model ramp: hold a fixed *arrival rate* per step rather than a fixed
      worker count, which is what coordinated-omission correction needs

## Step 3: client-side resilience, and how it backfires

The toolkit should let you test *policies*, not just endpoints.

- [ ] `retry` package: fixed / exponential backoff, with and without jitter
- [ ] `--retries N --backoff exponential --jitter full` on `run`
- [ ] Report retry amplification: requests sent vs. requests the caller made
- [ ] Demonstrate the retry storm — show how naive retries against a brownout
      multiply load and make recovery impossible, and how jitter and a retry
      budget fix it
- [ ] `breaker` package: closed → open → half-open state machine
- [ ] `--breaker` on `run`, reporting time spent open and requests shed
- [ ] Show that a breaker turns a slow failure into a fast one, which is the
      entire point

## Step 4: the fault-injecting proxy

The mock server only breaks requests it serves itself. A proxy can break
requests to a service you did not write.

- [ ] `shakedown proxy --listen :8080 --upstream http://real-service`
- [ ] Inject: added latency and jitter, error rates by status, connection
      resets, slow body transfer (bandwidth throttling), partial responses
- [ ] Target faults by path, method, or header
- [ ] Toggle faults at runtime over a control endpoint, so you can break a
      dependency while a test is running

## Step 5: make it usable by other people

- [ ] `scenario` files (YAML): multiple endpoints, weights, phases, assertions
- [ ] Machine-readable output: `--json`, and Prometheus text format
- [ ] Compare two runs: `shakedown diff before.json after.json`
- [ ] GitHub Action for reliability regression gates in CI
- [ ] Release binaries, Homebrew tap, `go install` path

## Later / maybe

- [ ] gRPC probe alongside HTTP
- [ ] Response validation (JSON schema, expected body) so "200" has to also mean "correct"
- [ ] Deterministic seeding for mock faults, for reproducible runs
- [ ] Coordinated-omission correction (open-model load generation)
- [ ] TUI live view
