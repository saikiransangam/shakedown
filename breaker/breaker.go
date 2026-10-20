// Package breaker implements a circuit breaker.
//
// The purpose of a breaker is not to reduce errors — a shed request is still a
// failed request. It is to convert a *slow* failure into a *fast* one. A
// caller waiting five seconds for a timeout holds a connection, a goroutine
// and often a lock for five seconds; thousands of those are how one sick
// dependency takes down everything that depends on it. Failing in microseconds
// instead keeps the caller healthy while the dependency is not.
package breaker

import (
	"sync"
	"time"
)

type State int

const (
	// StateClosed passes everything through and watches the failure rate.
	StateClosed State = iota
	// StateOpen rejects immediately, giving the target room to recover.
	StateOpen
	// StateHalfOpen lets a few probes through to find out whether it has.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

type Config struct {
	// FailureThreshold is the failure ratio (0..1) that trips the breaker.
	FailureThreshold float64
	// MinRequests is how many requests must be observed in the window before
	// the threshold is allowed to trip it. Without this, the first two
	// requests failing would open the breaker on a 100% failure rate.
	MinRequests int
	// Window is the rolling period the failure ratio is measured over.
	Window time.Duration
	// Cooldown is how long to stay open before probing.
	Cooldown time.Duration
	// HalfOpenProbes is how many consecutive probe successes close it again.
	HalfOpenProbes int
}

const (
	defaultThreshold      = 0.5
	defaultMinRequests    = 20
	defaultWindow         = 10 * time.Second
	defaultCooldown       = 2 * time.Second
	defaultHalfOpenProbes = 3
	windowBuckets         = 10
)

func (c Config) withDefaults() Config {
	if c.FailureThreshold <= 0 || c.FailureThreshold > 1 {
		c.FailureThreshold = defaultThreshold
	}
	if c.MinRequests <= 0 {
		c.MinRequests = defaultMinRequests
	}
	if c.Window <= 0 {
		c.Window = defaultWindow
	}
	if c.Cooldown <= 0 {
		c.Cooldown = defaultCooldown
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = defaultHalfOpenProbes
	}
	return c
}

// Transition records one state change, for the report.
type Transition struct {
	At   time.Time
	From State
	To   State
}

type Stats struct {
	State        State
	Shed         int64
	TimesOpened  int
	TimeOpen     time.Duration
	Transitions  []Transition
	FailureRatio float64
}

type bucket struct {
	total    int
	failures int
}

// Breaker is safe for concurrent use.
//
// Unlike the rest of this toolkit it is guarded by a mutex rather than owned
// by one goroutine: every worker must consult the same state on every request,
// so there is no way to avoid shared state here. The critical sections are a
// few arithmetic operations long.
type Breaker struct {
	cfg Config

	mu      sync.Mutex
	state   State
	buckets [windowBuckets]bucket
	cursor  int
	// bucketStart is when the bucket under the cursor began.
	bucketStart time.Time
	bucketWidth time.Duration

	openedAt     time.Time
	probesSent   int
	probesPassed int

	shed        int64
	timesOpened int
	timeOpen    time.Duration
	transitions []Transition
}

// maxTransitions caps the history kept for reporting; a flapping breaker
// should not grow memory without bound.
const maxTransitions = 64

func New(cfg Config) *Breaker {
	cfg = cfg.withDefaults()
	return &Breaker{
		cfg:         cfg,
		state:       StateClosed,
		bucketWidth: cfg.Window / windowBuckets,
	}
}

// Allow reports whether a request may be sent, and must be paired with a
// Record call when it returns true.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.advance(now)

	if b.state == StateOpen {
		if now.Sub(b.openedAt) < b.cfg.Cooldown {
			b.shed++
			return false
		}
		b.transition(now, StateHalfOpen)
	}

	if b.state == StateHalfOpen {
		// Admit exactly the number of probes needed to make a decision; the
		// rest are shed so a half-open breaker cannot itself become a load
		// spike against a target that has not recovered.
		if b.probesSent >= b.cfg.HalfOpenProbes {
			b.shed++
			return false
		}
		b.probesSent++
		return true
	}

	return true
}

// Record reports the outcome of a request that Allow permitted. fault should
// be true only for failures that indicate the target is unhealthy.
func (b *Breaker) Record(fault bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.advance(now)

	switch b.state {
	case StateClosed:
		b.buckets[b.cursor].total++
		if fault {
			b.buckets[b.cursor].failures++
		}
		total, failures := b.totals()
		if total >= b.cfg.MinRequests && float64(failures)/float64(total) >= b.cfg.FailureThreshold {
			b.open(now)
		}

	case StateHalfOpen:
		if fault {
			// One failed probe is enough: the target is still sick, and
			// another full window of traffic would only confirm it slowly.
			b.open(now)
			return
		}
		b.probesPassed++
		if b.probesPassed >= b.cfg.HalfOpenProbes {
			b.transition(now, StateClosed)
			b.resetWindow(now)
		}

	case StateOpen:
		// A result from a request that was in flight when the breaker opened.
		// Counting it would skew the window it is no longer part of.
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	timeOpen := b.timeOpen
	if b.state == StateOpen {
		timeOpen += time.Since(b.openedAt)
	}
	total, failures := b.totals()
	var ratio float64
	if total > 0 {
		ratio = float64(failures) / float64(total)
	}

	return Stats{
		State:        b.state,
		Shed:         b.shed,
		TimesOpened:  b.timesOpened,
		TimeOpen:     timeOpen,
		Transitions:  append([]Transition(nil), b.transitions...),
		FailureRatio: ratio,
	}
}

func (b *Breaker) open(now time.Time) {
	b.transition(now, StateOpen)
	b.openedAt = now
	b.timesOpened++
	b.resetWindow(now)
}

func (b *Breaker) transition(now time.Time, to State) {
	if b.state == to {
		return
	}
	if b.state == StateOpen && !b.openedAt.IsZero() {
		b.timeOpen += now.Sub(b.openedAt)
	}
	if len(b.transitions) < maxTransitions {
		b.transitions = append(b.transitions, Transition{At: now, From: b.state, To: to})
	}
	b.state = to
	b.probesSent = 0
	b.probesPassed = 0
}

func (b *Breaker) resetWindow(now time.Time) {
	b.buckets = [windowBuckets]bucket{}
	b.cursor = 0
	b.bucketStart = now
}

// advance rolls the window forward, clearing buckets that have aged out. A
// ring of buckets keeps this O(buckets) in the worst case and O(1) normally,
// without storing a timestamp per request.
func (b *Breaker) advance(now time.Time) {
	if b.bucketStart.IsZero() {
		b.bucketStart = now
		return
	}
	steps := int(now.Sub(b.bucketStart) / b.bucketWidth)
	if steps <= 0 {
		return
	}
	if steps > windowBuckets {
		steps = windowBuckets
	}
	for range steps {
		b.cursor = (b.cursor + 1) % windowBuckets
		b.buckets[b.cursor] = bucket{}
	}
	b.bucketStart = b.bucketStart.Add(time.Duration(steps) * b.bucketWidth)
}

func (b *Breaker) totals() (total, failures int) {
	for _, bk := range b.buckets {
		total += bk.total
		failures += bk.failures
	}
	return total, failures
}
