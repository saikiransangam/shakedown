package cli

import (
	"fmt"
	"net/http"
	"strings"
)

const Usage = `shakedown — find out why an API is unreliable

Usage:
  shakedown run   --target URL [flags]   send concurrent load and diagnose what breaks
  shakedown ramp  --target URL [flags]   sweep concurrency to find where it stops coping
  shakedown mock  [flags]                run a deliberately unreliable API to test against
  shakedown help                         show this message

Examples:
  # start an unreliable server, then point the driver at it
  shakedown mock --profile flaky
  shakedown run --target http://127.0.0.1:9090/ -c 50 -d 15s

  # find the service's capacity, and the load that still meets a 200ms p99
  shakedown ramp --target http://127.0.0.1:9090/ --to 128 --slo 200ms

  # measure a real service at a fixed, polite rate
  shakedown run --target https://api.example.com/health --rate 20 -d 1m
`

// headerFlag collects repeated -H flags, the way curl does.
type headerFlag struct {
	header http.Header
}

func (h *headerFlag) String() string { return "" }

func (h *headerFlag) Set(value string) error {
	name, val, found := strings.Cut(value, ":")
	if !found {
		return fmt.Errorf("header must be in 'Name: value' form, got %q", value)
	}
	if h.header == nil {
		h.header = make(http.Header)
	}
	h.header.Add(strings.TrimSpace(name), strings.TrimSpace(val))
	return nil
}
