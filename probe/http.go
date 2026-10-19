package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

type HTTPOptions struct {
	Method  string
	URL     string
	Header  http.Header
	Body    []byte
	Timeout time.Duration

	// PoolSize should match the run's concurrency. Go's default
	// MaxIdleConnsPerHost is 2, so without this a 50-worker run spends its
	// time opening and closing sockets and reports latency that belongs to
	// the load generator rather than the target.
	PoolSize int

	Insecure          bool
	DisableKeepAlives bool
	FollowRedirects   bool
}

// HTTP is a Probe that issues one HTTP request per call.
type HTTP struct {
	client *http.Client
	opts   HTTPOptions
}

const (
	defaultTimeout  = 10 * time.Second
	defaultPoolSize = 64
	dialTimeout     = 5 * time.Second
)

func NewHTTP(opts HTTPOptions) (*HTTP, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("target URL is required")
	}
	parsed, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse target %q: %w", opts.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("target must be http:// or https://, got %q", opts.URL)
	}
	if opts.Method == "" {
		opts.Method = http.MethodGet
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = defaultPoolSize
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          opts.PoolSize * 2,
		MaxIdleConnsPerHost:   opts.PoolSize,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     opts.DisableKeepAlives,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: opts.Insecure},
	}

	client := &http.Client{Transport: transport}
	if !opts.FollowRedirects {
		// Measure the endpoint that was asked for, not wherever it points.
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &HTTP{client: client, opts: opts}, nil
}

func (h *HTTP) Do(ctx context.Context) metrics.Result {
	// The deadline lives on the context rather than http.Client.Timeout so
	// that a timeout surfaces as context.DeadlineExceeded and can be told
	// apart from a connection failure.
	ctx, cancel := context.WithTimeout(ctx, h.opts.Timeout)
	defer cancel()

	tr := &tracer{}
	ctx = httptrace.WithClientTrace(ctx, tr.clientTrace())

	var body io.Reader
	if len(h.opts.Body) > 0 {
		body = bytes.NewReader(h.opts.Body)
	}

	start := time.Now()
	tr.setStart(start)

	req, err := http.NewRequestWithContext(ctx, h.opts.Method, h.opts.URL, body)
	if err != nil {
		return metrics.Result{
			Start:    start,
			Latency:  time.Since(start),
			Outcome:  metrics.OutcomeUnknown,
			Err:      err,
			Attempts: 1,
		}
	}
	if h.opts.Header != nil {
		req.Header = h.opts.Header.Clone()
	}
	if len(h.opts.Body) > 0 {
		req.ContentLength = int64(len(h.opts.Body))
	}

	resp, err := h.client.Do(req)
	if err != nil {
		now := time.Now()
		return metrics.Result{
			Start:    start,
			Latency:  now.Sub(start),
			Outcome:  metrics.Classify(0, err),
			Err:      err,
			Phases:   tr.phases(now),
			Attempts: 1,
		}
	}

	// Draining the body before closing it is what returns the connection to
	// the keep-alive pool; closing early forces a fresh TCP handshake for the
	// next request and quietly triples the measured latency.
	n, readErr := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	end := time.Now()

	outcome := metrics.Classify(resp.StatusCode, nil)
	if readErr != nil {
		// A 200 whose body died mid-transfer is not a success.
		outcome = metrics.Classify(0, readErr)
	}

	return metrics.Result{
		Start:    start,
		Latency:  end.Sub(start),
		Status:   resp.StatusCode,
		Outcome:  outcome,
		Err:      readErr,
		Phases:   tr.phases(end),
		Bytes:    n,
		Attempts: 1,
	}
}

// tracer collects per-phase timestamps from net/http/httptrace. The hooks can
// fire from the dialing goroutine, so the fields are mutex-guarded.
type tracer struct {
	mu        sync.Mutex
	start     time.Time
	dnsStart  time.Time
	dnsDone   time.Time
	connStart time.Time
	connDone  time.Time
	tlsStart  time.Time
	tlsDone   time.Time
	firstByte time.Time
	reused    bool
}

func (t *tracer) setStart(at time.Time) {
	t.mu.Lock()
	t.start = at
	t.mu.Unlock()
}

// stampFirst records only the earliest call, because ConnectStart can fire
// more than once for a single request when the dialer races IPv4 and IPv6.
func (t *tracer) stampFirst(field *time.Time) {
	t.mu.Lock()
	if field.IsZero() {
		*field = time.Now()
	}
	t.mu.Unlock()
}

func (t *tracer) stamp(field *time.Time) {
	t.mu.Lock()
	*field = time.Now()
	t.mu.Unlock()
}

func (t *tracer) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { t.stampFirst(&t.dnsStart) },
		DNSDone:              func(httptrace.DNSDoneInfo) { t.stamp(&t.dnsDone) },
		ConnectStart:         func(string, string) { t.stampFirst(&t.connStart) },
		ConnectDone:          func(string, string, error) { t.stamp(&t.connDone) },
		TLSHandshakeStart:    func() { t.stampFirst(&t.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.stamp(&t.tlsDone) },
		GotFirstResponseByte: func() { t.stamp(&t.firstByte) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			t.reused = info.Reused
			t.mu.Unlock()
		},
	}
}

func (t *tracer) phases(end time.Time) metrics.Phases {
	t.mu.Lock()
	defer t.mu.Unlock()

	p := metrics.Phases{Reused: t.reused}
	if !t.dnsStart.IsZero() && t.dnsDone.After(t.dnsStart) {
		p.DNS = t.dnsDone.Sub(t.dnsStart)
	}
	if !t.connStart.IsZero() && t.connDone.After(t.connStart) {
		p.Connect = t.connDone.Sub(t.connStart)
	}
	if !t.tlsStart.IsZero() && t.tlsDone.After(t.tlsStart) {
		p.TLS = t.tlsDone.Sub(t.tlsStart)
	}
	if !t.firstByte.IsZero() && !t.start.IsZero() {
		p.TTFB = t.firstByte.Sub(t.start)
		if end.After(t.firstByte) {
			p.Body = end.Sub(t.firstByte)
		}
	}
	return p
}
