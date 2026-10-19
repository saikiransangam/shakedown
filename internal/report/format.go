package report

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Dur formats a duration at a precision a human wants to read, unlike
// time.Duration.String which happily prints "1.234567ms".
func Dur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
}

// Pct formats a 0..1 ratio as a percentage.
func Pct(ratio float64) string {
	return fmt.Sprintf("%.1f%%", ratio*100)
}

func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value)
}

// Count renders an integer with thousands separators.
func Count(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return s + "," + strings.Join(parts, ",")
}

var sparkLevels = []rune("▁▂▃▄▅▆▇█")

// Spark renders a series as a one-line bar chart, scaled to its own maximum.
func Spark(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	max := values[0]
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	for _, v := range values {
		if max <= 0 {
			b.WriteRune(sparkLevels[0])
			continue
		}
		idx := int(v / max * float64(len(sparkLevels)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkLevels) {
			idx = len(sparkLevels) - 1
		}
		b.WriteRune(sparkLevels[idx])
	}
	return b.String()
}

// Bar renders a proportional bar for a 0..1 ratio.
func Bar(ratio float64, width int) string {
	if ratio < 0 {
		ratio = 0
	}
	filled := int(ratio*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled)
}

// IsTerminal reports whether f is attached to a terminal, which decides
// whether colour and in-place progress updates make sense.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Pad right-pads to a visible width, ignoring ANSI escapes. Using %-Ns on a
// coloured string counts the escape bytes and silently breaks every column.
func Pad(s string, width int) string {
	visible := len(ansiPattern.ReplaceAllString(s, ""))
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

type style struct {
	enabled bool
}

func newStyle(out *os.File) style {
	if os.Getenv("NO_COLOR") != "" {
		return style{}
	}
	return style{enabled: IsTerminal(out)}
}

func (s style) wrap(code, text string) string {
	if !s.enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) bold(text string) string  { return s.wrap("1", text) }
func (s style) dim(text string) string   { return s.wrap("2", text) }
func (s style) red(text string) string   { return s.wrap("31", text) }
func (s style) green(text string) string { return s.wrap("32", text) }
func (s style) amber(text string) string { return s.wrap("33", text) }
