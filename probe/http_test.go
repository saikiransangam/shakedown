package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saikiransangam/shakedown/metrics"
)

func newProbe(t *testing.T, opts HTTPOptions) *HTTP {
	t.Helper()
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	if opts.PoolSize == 0 {
		opts.PoolSize = 4
	}
	p, err := NewHTTP(opts)
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	return p
}

func TestRejectsBadTargets(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"no scheme":   "127.0.0.1:9090",
		"ftp scheme":  "ftp://example.com",
		"bad control": "http://exa\x7fmple.com",
	}
	for name, target := range cases {
		if _, err := NewHTTP(HTTPOptions{URL: target}); err == nil {
			t.Errorf("%s: expected NewHTTP(%q) to fail", name, target)
		}
	}
}

// The status-to-outcome mapping is the tool's central claim, so it is pinned
// against a real server rather than only unit-tested on Classify.
func TestClassifiesStatusCodes(t *testing.T) {
	cases := []struct {
		status int
		want   metrics.Outcome
	}{
		{200, metrics.OutcomeSuccess},
		{204, metrics.OutcomeSuccess},
		{301, metrics.OutcomeSuccess}, // a redirect we deliberately do not follow
		{400, metrics.OutcomeClientError},
		{404, metrics.OutcomeClientError},
		{429, metrics.OutcomeClientError},
		{500, metrics.OutcomeServerError},
		{503, metrics.OutcomeServerError},
	}

	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		}))
		res := newProbe(t, HTTPOptions{URL: srv.URL}).Do(context.Background())
		srv.Close()

		if res.Outcome != c.want {
			t.Errorf("status %d: outcome = %q, want %q", c.status, res.Outcome, c.want)
		}
		if res.Status != c.status {
			t.Errorf("status %d: reported %d", c.status, res.Status)
		}
	}
}

func TestSuccessfulRequestRecordsBytesAndPhases(t *testing.T) {
	payload := []byte("hello reliability")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	res := newProbe(t, HTTPOptions{URL: srv.URL}).Do(context.Background())

	if res.Outcome != metrics.OutcomeSuccess {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	if res.Bytes != int64(len(payload)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(payload))
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if res.Latency <= 0 {
		t.Error("latency was not measured")
	}
	if res.Phases.TTFB <= 0 {
		t.Error("TTFB was not measured")
	}
	// A fresh connection must report the handshake it actually performed.
	if res.Phases.Connect <= 0 {
		t.Error("Connect was not measured on a cold connection")
	}
	if res.Phases.Reused {
		t.Error("the first request on a new client cannot have reused a connection")
	}
}

func TestTimeoutIsClassifiedAsTimeoutNotAConnectionError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the request open past the client's deadline
	}))
	// Releasing before Close keeps the server's own shutdown from blocking.
	defer func() { close(release); srv.Close() }()

	res := newProbe(t, HTTPOptions{URL: srv.URL, Timeout: 100 * time.Millisecond}).
		Do(context.Background())

	if res.Outcome != metrics.OutcomeTimeout {
		t.Errorf("outcome = %q, want timeout", res.Outcome)
	}
	if res.Status != 0 {
		t.Errorf("Status = %d, want 0 — no response arrived", res.Status)
	}
}

// A server that accepts and hangs up without answering is the signature of a
// crash or a connection limit, and must not be reported as a timeout.
func TestAbruptCloseIsClassifiedAsConnReset(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	res := newProbe(t, HTTPOptions{URL: "http://" + ln.Addr().String()}).Do(context.Background())

	if res.Outcome != metrics.OutcomeConnReset {
		t.Errorf("outcome = %q, want conn_reset", res.Outcome)
	}
}

func TestRefusedConnectionIsClassifiedAsConnRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening there any more

	res := newProbe(t, HTTPOptions{URL: "http://" + addr}).Do(context.Background())

	if res.Outcome != metrics.OutcomeConnRefused {
		t.Errorf("outcome = %q, want conn_refused", res.Outcome)
	}
}

func TestCanceledParentContextIsNotCountedAsAFailure(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	res := newProbe(t, HTTPOptions{URL: srv.URL, Timeout: 5 * time.Second}).Do(ctx)

	// This distinction is what stops Ctrl-C from inventing a burst of
	// failures and changing a run's verdict.
	if res.Outcome != metrics.OutcomeCanceled {
		t.Errorf("outcome = %q, want canceled", res.Outcome)
	}
}

// Draining the body is what returns a connection to the pool. If this breaks,
// every request pays for a fresh handshake and the tool measures itself.
func TestKeepAliveConnectionsAreReused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("body that must be drained"))
	}))
	defer srv.Close()

	p := newProbe(t, HTTPOptions{URL: srv.URL})
	if res := p.Do(context.Background()); res.Phases.Reused {
		t.Fatal("first request should not be a reuse")
	}

	for i := range 5 {
		res := p.Do(context.Background())
		if !res.Phases.Reused {
			t.Fatalf("request %d did not reuse the connection", i+2)
		}
		// A reused connection does no DNS lookup or handshake, and reporting
		// zeros as real measurements would drag those medians to nothing.
		if res.Phases.Connect != 0 || res.Phases.DNS != 0 {
			t.Errorf("request %d reported setup phases on a reused connection", i+2)
		}
	}
}

func TestDisableKeepAlivesForcesFreshConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	p := newProbe(t, HTTPOptions{URL: srv.URL, DisableKeepAlives: true})
	p.Do(context.Background())
	res := p.Do(context.Background())

	if res.Phases.Reused {
		t.Error("--no-keepalive must not reuse connections")
	}
	if res.Phases.Connect <= 0 {
		t.Error("a fresh connection should report its handshake")
	}
}

// Every call must send the request body. A reader created once and shared
// across calls would be drained after the first, silently sending empty
// bodies for the rest of the run.
func TestBodyIsSentOnEveryRequest(t *testing.T) {
	var bodies atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) == "payload" {
			bodies.Add(1)
		}
		if r.ContentLength != int64(len("payload")) {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len("payload"))
		}
	}))
	defer srv.Close()

	p := newProbe(t, HTTPOptions{
		Method: http.MethodPost,
		URL:    srv.URL,
		Body:   []byte("payload"),
	})
	for range 3 {
		if res := p.Do(context.Background()); res.Outcome != metrics.OutcomeSuccess {
			t.Fatalf("outcome = %q", res.Outcome)
		}
	}

	if bodies.Load() != 3 {
		t.Errorf("server received the body %d times, want 3", bodies.Load())
	}
}

func TestMethodAndHeadersAreSent(t *testing.T) {
	var seen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.Header.Get("X-Trace") == "abc" {
			seen.Store(true)
		}
	}))
	defer srv.Close()

	header := http.Header{}
	header.Set("X-Trace", "abc")
	newProbe(t, HTTPOptions{Method: http.MethodDelete, URL: srv.URL, Header: header}).
		Do(context.Background())

	if !seen.Load() {
		t.Error("method or header did not reach the server")
	}
}

func TestRedirectsAreNotFollowedByDefault(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			hits.Add(1)
			w.Write([]byte("arrived"))
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	defer srv.Close()

	res := newProbe(t, HTTPOptions{URL: srv.URL + "/start"}).Do(context.Background())
	if res.Status != http.StatusFound {
		t.Errorf("Status = %d, want 302 — we measure the endpoint we asked for", res.Status)
	}
	if hits.Load() != 0 {
		t.Error("the redirect was followed despite --follow-redirects being off")
	}

	res = newProbe(t, HTTPOptions{URL: srv.URL + "/start", FollowRedirects: true}).
		Do(context.Background())
	if res.Status != http.StatusOK {
		t.Errorf("with --follow-redirects, Status = %d, want 200", res.Status)
	}
	if hits.Load() != 1 {
		t.Error("the redirect was not followed when asked for")
	}
}

// A response whose body dies partway through is not a success, however the
// status line read.
func TestTruncatedBodyOverridesA200(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nonly-ten!!")
				conn.Close() // 90 bytes short of what was promised
			}()
		}
	}()

	res := newProbe(t, HTTPOptions{URL: "http://" + ln.Addr().String()}).Do(context.Background())

	if res.Outcome.OK() {
		t.Errorf("outcome = %q, want a failure for a truncated body", res.Outcome)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d; the status line really was 200 and should be reported", res.Status)
	}
}
