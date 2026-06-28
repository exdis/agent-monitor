package tui

import (
	"fmt"
	"strconv"
	"time"
)

// itoa is a tiny int-to-string helper used throughout the views.
func itoa(n int) string { return strconv.Itoa(n) }

// humanAgo renders a compact relative duration like "3s", "5m", "2h".
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// truncate shortens s to max runes, adding an ellipsis when cut.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// pad right-pads (or truncates) s to exactly width runes.
func pad(s string, width int) string {
	s = truncate(s, width)
	if n := width - len([]rune(s)); n > 0 {
		return s + spaces(n)
	}
	return s
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// shortDir keeps the last two path segments for compactness.
func shortDir(p string) string {
	if p == "" {
		return ""
	}
	segs := splitPath(p)
	if len(segs) <= 2 {
		return p
	}
	return ".../" + segs[len(segs)-2] + "/" + segs[len(segs)-1]
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, c := range p {
		if c == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
