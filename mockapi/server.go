// Package mockapi serves a deliberately unreliable HTTP API so that
// reliability tooling has something to measure before it is pointed at
// anything that matters.
package mockapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"sync/atomic"
	"time"
)

// Profile describes how a mock endpoint misbehaves.
type Profile struct {
	Name        string
	Description string

	BaseLatency time.Duration
	Jitter      time.Duration // uniform +/- added to BaseLatency

	ErrorRate float64 // share of requests answered with 503
	ResetRate float64 // share of requests whose TCP connection is killed
	HangRate  float64 // share of requests held open for HangFor
	HangFor   time.Duration

	// Capacity models a server that can only work on so many requests at
	// once. Past it, each queued request adds QueueCost to everyone's
	// latency, which is how a healthy p50 turns into a terrible p99.
	Capacity  int
	QueueCost time.Duration

	// ShedAbove answers 503 immediately once in-flight requests exceed it,
	// imitating a load shedder. 0 disables shedding.
	ShedAbove int
}

var profiles = map[string]Profile{
	"healthy": {
		Name:        "healthy",
		Description: "fast and boring: a baseline to check the tool itself",
		BaseLatency: 5 * time.Millisecond,
		Jitter:      3 * time.Millisecond,
	},
	"slow": {
		Name:        "slow",
		Description: "always succeeds, always slow",
		BaseLatency: 400 * time.Millisecond,
		Jitter:      150 * time.Millisecond,
	},
	"flaky": {
		Name:        "flaky",
		Description: "mostly fine, with 503s, hangs and dropped connections mixed in",
		BaseLatency: 40 * time.Millisecond,
		Jitter:      30 * time.Millisecond,
		ErrorRate:   0.10,
		ResetRate:   0.02,
		HangRate:    0.03,
		HangFor:     5 * time.Second,
	},
	"brownout": {
		Name:        "brownout",
		Description: "fast when idle; latency climbs with load, then it sheds",
		BaseLatency: 20 * time.Millisecond,
		Jitter:      10 * time.Millisecond,
		Capacity:    10,
		QueueCost:   8 * time.Millisecond,
		// Shedding must start well above Capacity, otherwise the queueing it
		// is meant to protect against never becomes visible.
		ShedAbove: 150,
	},
	"tarpit": {
		Name:        "tarpit",
		Description: "accepts every connection and never answers",
		HangRate:    1.0,
		HangFor:     60 * time.Second,
	},
}

// ProfileNames returns the available profile names in alphabetical order.
func ProfileNames() []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LookupProfile returns the named profile.
func LookupProfile(name string) (Profile, error) {
	p, ok := profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown profile %q (have: %v)", name, ProfileNames())
	}
	return p, nil
}

// Describe returns a human-readable list of profiles.
func Describe() string {
	out := ""
	for _, name := range ProfileNames() {
		out += fmt.Sprintf("  %-9s %s\n", name, profiles[name].Description)
	}
	return out
}

// Server is an HTTP server that applies a Profile to every request.
type Server struct {
	profile  Profile
	inflight atomic.Int64
	served   atomic.Int64
}

func New(p Profile) *Server { return &Server{profile: p} }

// Serve listens on addr until ctx is canceled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handle)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

// Served is the number of requests the server has handled.
func (s *Server) Served() int64 { return s.served.Load() }

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// Health checks stay honest even while the main handler misbehaves, which
	// is exactly how a real service hides a brownout from its load balancer.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","inflight":%d}`+"\n", s.inflight.Load())
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	inflight := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	s.served.Add(1)

	p := s.profile

	if p.ShedAbove > 0 && int(inflight) > p.ShedAbove {
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
		return
	}

	if p.ResetRate > 0 && rand.Float64() < p.ResetRate {
		resetConnection(w)
		return
	}

	if p.HangRate > 0 && rand.Float64() < p.HangRate {
		sleepCtx(r.Context(), p.HangFor)
		return
	}

	latency := p.BaseLatency
	if p.Jitter > 0 {
		latency += time.Duration(rand.Int64N(int64(2*p.Jitter))) - p.Jitter
	}
	if p.Capacity > 0 && int(inflight) > p.Capacity {
		queued := int(inflight) - p.Capacity
		latency += time.Duration(queued) * p.QueueCost
	}
	if latency < 0 {
		latency = 0
	}
	if !sleepCtx(r.Context(), latency) {
		return // client gave up while we were "working"
	}

	if p.ErrorRate > 0 && rand.Float64() < p.ErrorRate {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"error":"dependency unavailable"}`)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"profile":    p.Name,
		"inflight":   inflight,
		"latency_ms": latency.Milliseconds(),
		"path":       r.URL.Path,
	})
}

// resetConnection kills the TCP connection without a response, producing a
// real "connection reset by peer" on the client.
func resetConnection(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	// SetLinger(0) makes Close send an RST instead of a graceful FIN, which
	// is the difference between the client seeing EOF and seeing ECONNRESET.
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// sleepCtx reports whether the sleep finished without the client leaving.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
