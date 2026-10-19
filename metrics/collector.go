package metrics

import (
	"sort"
	"sync/atomic"
	"time"
)

// LiveStats is a cheap snapshot for progress output while a run is in flight.
type LiveStats struct {
	Sent    int64
	Failed  int64
	Elapsed time.Duration
}

// Bucket aggregates one second of a run, which is what lets us see whether a
// service degraded over time instead of being uniformly bad.
type Bucket struct {
	Second  int
	Count   int
	Failed  int
	Latency Distribution
}

// Summary is the full result of a run.
type Summary struct {
	Target      string
	Concurrency int
	RateLimit   float64
	Start       time.Time
	End         time.Time
	Interrupted bool

	Total    int
	Outcomes map[Outcome]int
	Statuses map[int]int

	Success Distribution // latency of successful requests only
	All     Distribution // latency of every request, failures included

	DNS     Distribution
	Connect Distribution
	TLS     Distribution
	TTFB    Distribution
	Body    Distribution

	Reused  int
	Bytes   int64
	Buckets []Bucket
}

func (s *Summary) Elapsed() time.Duration {
	if s.End.Before(s.Start) {
		return 0
	}
	return s.End.Sub(s.Start)
}

func (s *Summary) Throughput() float64 {
	secs := s.Elapsed().Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(s.Total) / secs
}

func (s *Summary) Failures() int {
	return s.Total - s.Outcomes[OutcomeSuccess]
}

// FailureRate is in the range 0..1.
func (s *Summary) FailureRate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Failures()) / float64(s.Total)
}

// Rate is the share of the run that ended in the given outcome, 0..1.
func (s *Summary) Rate(o Outcome) float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Outcomes[o]) / float64(s.Total)
}

func (s *Summary) ReuseRate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Reused) / float64(s.Total)
}

type bucketAcc struct {
	count   int
	failed  int
	samples []time.Duration
}

// Collector aggregates results from a run.
//
// Exactly one goroutine (the one inside Collect) touches the maps and slices,
// so none of them need a lock. Only the two live counters are atomic, because
// a progress printer reads them concurrently.
type Collector struct {
	sent   atomic.Int64
	failed atomic.Int64

	start time.Time
	last  time.Time

	outcomes map[Outcome]int
	statuses map[int]int

	success []time.Duration
	all     []time.Duration

	dns     []time.Duration
	connect []time.Duration
	tlsTime []time.Duration
	ttfb    []time.Duration
	body    []time.Duration

	reused  int
	bytes   int64
	buckets map[int]*bucketAcc
}

func NewCollector(start time.Time) *Collector {
	return &Collector{
		start:    start,
		outcomes: make(map[Outcome]int),
		statuses: make(map[int]int),
		buckets:  make(map[int]*bucketAcc),
	}
}

// Collect drains ch until it is closed, then returns the finished summary.
func (c *Collector) Collect(ch <-chan Result) *Summary {
	for res := range ch {
		c.add(res)
	}
	return c.summary()
}

// Live is safe to call from another goroutine while Collect is running.
func (c *Collector) Live() LiveStats {
	return LiveStats{
		Sent:    c.sent.Load(),
		Failed:  c.failed.Load(),
		Elapsed: time.Since(c.start),
	}
}

func (c *Collector) add(r Result) {
	c.sent.Add(1)
	if !r.Outcome.OK() {
		c.failed.Add(1)
	}

	c.outcomes[r.Outcome]++
	if r.Status != 0 {
		c.statuses[r.Status]++
	}
	c.all = append(c.all, r.Latency)
	if r.Outcome.OK() {
		c.success = append(c.success, r.Latency)
	}
	c.bytes += r.Bytes

	// Zero phases mean the phase did not happen (a reused connection does no
	// DNS lookup), so skip them rather than dragging the median to zero.
	if r.Phases.DNS > 0 {
		c.dns = append(c.dns, r.Phases.DNS)
	}
	if r.Phases.Connect > 0 {
		c.connect = append(c.connect, r.Phases.Connect)
	}
	if r.Phases.TLS > 0 {
		c.tlsTime = append(c.tlsTime, r.Phases.TLS)
	}
	if r.Phases.TTFB > 0 {
		c.ttfb = append(c.ttfb, r.Phases.TTFB)
	}
	if r.Phases.Body > 0 {
		c.body = append(c.body, r.Phases.Body)
	}
	if r.Phases.Reused {
		c.reused++
	}

	sec := int(r.Start.Sub(c.start).Seconds())
	if sec < 0 {
		sec = 0
	}
	b := c.buckets[sec]
	if b == nil {
		b = &bucketAcc{}
		c.buckets[sec] = b
	}
	b.count++
	if !r.Outcome.OK() {
		b.failed++
	}
	b.samples = append(b.samples, r.Latency)

	if end := r.Start.Add(r.Latency); end.After(c.last) {
		c.last = end
	}
}

func (c *Collector) summary() *Summary {
	end := c.last
	if end.IsZero() {
		end = time.Now()
	}

	s := &Summary{
		Start:    c.start,
		End:      end,
		Total:    len(c.all),
		Outcomes: c.outcomes,
		Statuses: c.statuses,
		Success:  NewDistribution(c.success),
		All:      NewDistribution(c.all),
		DNS:      NewDistribution(c.dns),
		Connect:  NewDistribution(c.connect),
		TLS:      NewDistribution(c.tlsTime),
		TTFB:     NewDistribution(c.ttfb),
		Body:     NewDistribution(c.body),
		Reused:   c.reused,
		Bytes:    c.bytes,
	}

	seconds := make([]int, 0, len(c.buckets))
	for sec := range c.buckets {
		seconds = append(seconds, sec)
	}
	sort.Ints(seconds)
	for _, sec := range seconds {
		acc := c.buckets[sec]
		s.Buckets = append(s.Buckets, Bucket{
			Second:  sec,
			Count:   acc.count,
			Failed:  acc.failed,
			Latency: NewDistribution(acc.samples),
		})
	}
	return s
}
