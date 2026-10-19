package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func TestClassifyStatusCodes(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{
		{200, OutcomeSuccess},
		{204, OutcomeSuccess},
		{301, OutcomeSuccess},
		{404, OutcomeClientError},
		{429, OutcomeClientError},
		{500, OutcomeServerError},
		{503, OutcomeServerError},
	}
	for _, c := range cases {
		if got := Classify(c.status, nil); got != c.want {
			t.Errorf("Classify(%d, nil) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestClassifyErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Outcome
	}{
		{"deadline", context.DeadlineExceeded, OutcomeTimeout},
		{"wrapped deadline", fmt.Errorf("Get %q: %w", "http://x", context.DeadlineExceeded), OutcomeTimeout},
		{"canceled", context.Canceled, OutcomeCanceled},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, OutcomeConnRefused},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, OutcomeConnReset},
		{"eof", io.EOF, OutcomeConnReset},
		{"unexpected eof", io.ErrUnexpectedEOF, OutcomeConnReset},
		{"dns", &net.DNSError{Err: "no such host", Name: "nope.invalid"}, OutcomeDNSFailure},
		{"mystery", errors.New("something nobody anticipated"), OutcomeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(0, c.err); got != c.want {
				t.Errorf("Classify(0, %v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestClassifyRealTimeout drives a real client against a real server to
// confirm the error net/http actually produces is classified as a timeout,
// since that error's shape is not obvious from the documentation.
func TestClassifyRealTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.Client().Do(req)
	if err == nil {
		t.Fatal("expected the request to time out")
	}
	if got := Classify(0, err); got != OutcomeTimeout {
		t.Errorf("Classify(%v) = %q, want %q", err, got, OutcomeTimeout)
	}
}

// TestClassifyRealConnRefused confirms the same for a closed port.
func TestClassifyRealConnRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	_, err = http.Get("http://" + addr)
	if err == nil {
		t.Skip("something else grabbed the port")
	}
	if got := Classify(0, err); got != OutcomeConnRefused {
		t.Errorf("Classify(%v) = %q, want %q", err, got, OutcomeConnRefused)
	}
}
