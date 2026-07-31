package ui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// sparkChars run from lowest to highest; index 0 is used for zero.
var sparkChars = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values as a fixed-width run of block characters, padded on
// the left so a host with little history stays column-aligned with one that has
// filled its buffer.
//
// The scale is fixed to [0,max] rather than auto-ranged to the data: an
// auto-ranged sparkline makes idle noise look like a crisis, which is exactly
// the wrong signal for a monitoring tool.
func sparkline(values []float64, width int, max float64) string {
	if width <= 0 {
		return ""
	}
	if max <= 0 {
		max = 100
	}

	// Show the most recent `width` samples.
	if len(values) > width {
		values = values[len(values)-width:]
	}

	var b strings.Builder
	for i := 0; i < width-len(values); i++ {
		b.WriteRune(' ')
	}
	for _, v := range values {
		b.WriteRune(sparkChar(v, max))
	}
	return b.String()
}

func sparkChar(v, max float64) rune {
	if math.IsNaN(v) || v <= 0 {
		return sparkChars[0]
	}
	idx := int(v / max * float64(len(sparkChars)))
	if idx >= len(sparkChars) {
		idx = len(sparkChars) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sparkChars[idx]
}

// bar renders a proportional bar for the detail view.
func bar(pct float64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// humanBytes formats a byte count with a binary-ish suffix, kept to three
// significant characters so columns stay narrow.
func humanBytes(b uint64) string {
	const unit = 1024.0
	v := float64(b)
	if v < unit {
		return fmt.Sprintf("%dB", b)
	}
	units := []string{"K", "M", "G", "T", "P"}
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f%s", v, units[i])
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

// humanKB formats a kilobyte count (df and RSS both report KB).
func humanKB(kb uint64) string { return humanBytes(kb * 1024) }

// humanRate formats a bytes-per-second reading.
func humanRate(bps float64) string {
	if bps < 0 {
		return "—"
	}
	return humanBytes(uint64(bps)) + "/s"
}

// humanDuration renders an uptime in the largest two units that matter, which
// is how you actually read "how long has this been up".
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// truncate shortens s to width, marking elision with an ellipsis so a cut
// command name is visibly cut rather than silently wrong.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// padRight left-aligns s in a field of the given width.
func padRight(s string, width int) string {
	n := width - len([]rune(s))
	if n <= 0 {
		return s
	}
	return s + strings.Repeat(" ", n)
}

// padLeft right-aligns s, which is what numeric columns want so digits line up.
func padLeft(s string, width int) string {
	n := width - len([]rune(s))
	if n <= 0 {
		return s
	}
	return strings.Repeat(" ", n) + s
}
