package metrics

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
)

// Outcome is the classification of one request attempt.
//
// The taxonomy is the point of this package: "error rate: 8%" tells you nothing
// actionable, while "6% timeouts, 2% connection resets" points at a specific
// failure mode and therefore a specific fix.
type Outcome string

const (
	OutcomeSuccess     Outcome = "success"      // 1xx-3xx
	OutcomeClientError Outcome = "client_error" // 4xx: usually our fault, not the server's
	OutcomeServerError Outcome = "server_error" // 5xx: the server knew it failed
	OutcomeTimeout     Outcome = "timeout"      // no answer inside our deadline
	OutcomeConnRefused Outcome = "conn_refused" // nothing listening, or accept queue full
	OutcomeConnReset   Outcome = "conn_reset"   // connection died mid-flight
	OutcomeDNSFailure  Outcome = "dns_failure"
	OutcomeTLSFailure  Outcome = "tls_failure"
	OutcomeCanceled    Outcome = "canceled"     // we gave up, not the server
	OutcomeCircuitOpen Outcome = "circuit_open" // never sent: our own breaker shed it
	OutcomeUnknown     Outcome = "unknown"
)

// AllOutcomes is the stable order outcomes are reported in: successes first,
// then failures roughly in order of how far the request got.
func AllOutcomes() []Outcome {
	return []Outcome{
		OutcomeSuccess,
		OutcomeClientError,
		OutcomeServerError,
		OutcomeTimeout,
		OutcomeConnReset,
		OutcomeConnRefused,
		OutcomeDNSFailure,
		OutcomeTLSFailure,
		OutcomeCircuitOpen,
		OutcomeCanceled,
		OutcomeUnknown,
	}
}

// OK reports whether the outcome counts as a successful request.
func (o Outcome) OK() bool { return o == OutcomeSuccess }

// Retryable reports whether a retry could plausibly fix this failure.
//
// A 4xx will return 4xx again, and a bad certificate will not repair itself,
// so retrying those only multiplies load for nothing. This distinction is what
// separates a retry policy from a denial-of-service attack on your own
// dependency.
func (o Outcome) Retryable() bool {
	switch o {
	case OutcomeTimeout, OutcomeConnReset, OutcomeConnRefused,
		OutcomeServerError, OutcomeDNSFailure:
		return true
	default:
		return false
	}
}

// ServerFault reports whether the outcome says the target is unhealthy, as
// opposed to the request being wrong or the caller giving up. A circuit
// breaker must only count these: tripping on 4xx would let one caller's bad
// request cut off everyone else.
func (o Outcome) ServerFault() bool {
	switch o {
	case OutcomeTimeout, OutcomeConnReset, OutcomeConnRefused,
		OutcomeServerError, OutcomeDNSFailure, OutcomeTLSFailure:
		return true
	default:
		return false
	}
}

// Classify turns an HTTP status and/or transport error into an Outcome.
//
// Pass status 0 when the request never produced a response.
func Classify(status int, err error) Outcome {
	if err == nil {
		switch {
		case status >= 500:
			return OutcomeServerError
		case status >= 400:
			return OutcomeClientError
		default:
			return OutcomeSuccess
		}
	}

	// Deadline checks come first: a timeout often arrives wrapped in a
	// net.OpError that would otherwise match a coarser case below.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return OutcomeTimeout
	}
	if errors.Is(err, context.Canceled) {
		return OutcomeCanceled
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return OutcomeTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return OutcomeDNSFailure
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return OutcomeTLSFailure
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return OutcomeTLSFailure
	}

	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return OutcomeConnRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return OutcomeConnReset
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return OutcomeConnReset
	}

	// Some net/http transport errors carry no sentinel we can match on, only
	// text. Matching on strings is ugly but it is the difference between
	// "unknown: 4%" and a usable diagnosis.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "x509:"), strings.Contains(msg, "tls:"):
		return OutcomeTLSFailure
	case strings.Contains(msg, "no such host"):
		return OutcomeDNSFailure
	case strings.Contains(msg, "connection refused"):
		return OutcomeConnRefused
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "EOF"),
		strings.Contains(msg, "server closed"),
		strings.Contains(msg, "broken pipe"):
		return OutcomeConnReset
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return OutcomeTimeout
	case strings.Contains(msg, "context canceled"):
		return OutcomeCanceled
	}
	return OutcomeUnknown
}
