// Package ramp sweeps a target across increasing concurrency levels to find
// the point where it stops coping.
//
// A single load run tells you how a service behaves at one concurrency level.
// That is rarely the question you have — you want to know where the service's
// limit is, and a limit is only visible by comparing levels against each other.
package ramp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/saikiransangam/shakedown/load"
	"github.com/saikiransangam/shakedown/metrics"
)

// Step is one concurrency level and what it measured.
type Step struct {
	Concurrency int
	Summary     *metrics.Summary
}

// ProbeFactory builds a probe for a given concurrency level. Each level needs
// its own probe because the connection pool must be sized to that level.
type ProbeFactory func(concurrency int) (load.Probe, error)

type Config struct {
	Levels       []int
	StepDuration time.Duration
	// Warmup runs unrecorded load before each step so that connection
	// handshakes and cold caches are not charged to the measurement.
	Warmup time.Duration
}

type Result struct {
	Target      string
	Steps       []Step
	Interrupted bool
}

const (
	defaultStepDuration = 5 * time.Second
	defaultWarmup       = time.Second
)

// Run executes each concurrency level in turn. onStep, if non-nil, is called
// after each level completes so a caller can report progress during a sweep
// that may take a minute.
func Run(ctx context.Context, cfg Config, newProbe ProbeFactory, onStep func(Step)) (*Result, error) {
	if len(cfg.Levels) == 0 {
		return nil, fmt.Errorf("no concurrency levels to run")
	}
	if cfg.StepDuration <= 0 {
		cfg.StepDuration = defaultStepDuration
	}

	result := &Result{}
	for _, concurrency := range cfg.Levels {
		if ctx.Err() != nil {
			result.Interrupted = true
			break
		}

		probe, err := newProbe(concurrency)
		if err != nil {
			return nil, err
		}

		if cfg.Warmup > 0 {
			// Discarded on purpose: this run exists only to warm the pool.
			load.Run(ctx, load.Config{Concurrency: concurrency, Duration: cfg.Warmup}, probe)
		}
		if ctx.Err() != nil {
			result.Interrupted = true
			break
		}

		summary := load.Run(ctx, load.Config{
			Concurrency: concurrency,
			Duration:    cfg.StepDuration,
		}, probe)

		step := Step{Concurrency: concurrency, Summary: summary}
		result.Steps = append(result.Steps, step)
		if onStep != nil {
			onStep(step)
		}
	}

	if ctx.Err() != nil {
		result.Interrupted = true
	}
	return result, nil
}

// GeometricLevels returns doubling concurrency levels from `from` up to `to`,
// always including `to`. Doubling rather than stepping linearly is deliberate:
// saturation points span orders of magnitude, so a linear sweep either misses
// a low limit or wastes most of its steps above a high one.
func GeometricLevels(from, to int) []int {
	if from < 1 {
		from = 1
	}
	if to < from {
		to = from
	}
	var levels []int
	for c := from; c < to; c *= 2 {
		levels = append(levels, c)
	}
	return append(levels, to)
}

// ParseLevels reads an explicit comma-separated level list, e.g. "1,10,50".
func ParseLevels(spec string) ([]int, error) {
	fields := strings.Split(spec, ",")
	levels := make([]int, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("invalid concurrency level %q", field)
		}
		if n < 1 {
			return nil, fmt.Errorf("concurrency level must be at least 1, got %d", n)
		}
		levels = append(levels, n)
	}
	if len(levels) == 0 {
		return nil, fmt.Errorf("no concurrency levels given")
	}
	return levels, nil
}
