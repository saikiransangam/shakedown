package metrics

import "time"

// Phases splits one request's latency into the stages a request actually goes
// through. This is how you tell "the network is slow" apart from "the server is
// slow": if TTFB dominates, the server is thinking; if connect dominates, the
// problem is the path to it or its accept queue.
type Phases struct {
	DNS     time.Duration
	Connect time.Duration
	TLS     time.Duration
	TTFB    time.Duration // request start until the first response byte
	Body    time.Duration // first byte until the body is fully read
	Reused  bool          // connection came from the keep-alive pool
}

// Result is everything we learned from a single request attempt.
type Result struct {
	Start    time.Time
	Latency  time.Duration
	Status   int
	Outcome  Outcome
	Err      error
	Phases   Phases
	Bytes    int64
	Attempts int // 1 unless a retry policy was in play
}
