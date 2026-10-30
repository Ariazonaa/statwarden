// Package badge renders small Shields-style SVG status and uptime badges for
// embedding in READMEs and dashboards. Output is a self-contained SVG string;
// rendering is pure so it is fully unit-tested.
package badge

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Colors (Shields.io palette).
const (
	colorGreen       = "#4c1"
	colorGreenYellow = "#a3c51c"
	colorYellow      = "#dfb317"
	colorOrange      = "#fe7d37"
	colorRed         = "#e05d44"
	colorGray        = "#9f9f9f"
	colorBlue        = "#007ec6"
	labelBg          = "#555"
)

// statusColor maps a monitor status string to a badge color.
func statusColor(status string) string {
	switch status {
	case "up":
		return colorGreen
	case "down":
		return colorRed
	case "pending":
		return colorYellow
	case "maintenance":
		return colorBlue
	case "paused":
		return colorGray
	default:
		return colorGray
	}
}

// uptimeColor grades an uptime percentage (0..100) into a color.
func uptimeColor(pct float64) string {
	switch {
	case pct >= 99:
		return colorGreen
	case pct >= 95:
		return colorGreenYellow
	case pct >= 90:
		return colorYellow
	case pct >= 75:
		return colorOrange
	default:
		return colorRed
	}
}

// Status renders a status badge ("label | value"), e.g. "status | up".
func Status(label, status string) string {
	return render(label, status, statusColor(status))
}

// Uptime renders an uptime badge, e.g. "uptime | 99.95%". A negative pct (no
// data) renders as "n/a" in gray.
func Uptime(label string, pct float64) string {
	if pct < 0 {
		return render(label, "n/a", colorGray)
	}
	// TRUNCATED, not rounded.
	//
	// FormatFloat rounds, so 99.995 rendered as "100.00%" — and this is the
	// number people paste into a README. The availability archive already
	// truncates for exactly this reason, with a comment calling a month that had
	// downtime in it reported as perfect "the one number this page must not
	// produce"; the badge and the status page disagreed with it. Truncation can
	// only ever understate, so 100.00% now means what it says.
	value := strconv.FormatFloat(math.Floor(pct*100)/100, 'f', 2, 64) + "%"
	return render(label, value, uptimeColor(pct))
}

// maxPanelRunes bounds the text in either badge panel.
//
// The label arrives from ?label= on public, unauthenticated endpoints, and a
// badge is the highest-volume public path in the app (READMEs embed it). Without
// a bound a query string near Go's 1 MiB header limit rendered a multi-MiB SVG:
// estWidth sizes the image from the text, escaping expands '&' to five bytes,
// and render writes each panel four times. That is response amplification, not
// injection — escape() was always correct.
//
// 64 is well past any label that fits a badge at 11px.
const maxPanelRunes = 64

// truncate shortens s to at most maxPanelRunes runes, marking the cut with an
// ellipsis.
//
// It runs on runes, not bytes: cutting a multi-byte character in half would emit
// an invalid UTF-8 sequence into the SVG. It also runs BEFORE escape — cutting
// escaped text could land inside an entity and leave a bare "&am" behind.
func truncate(s string) string {
	n := 0
	for i := range s {
		if n == maxPanelRunes {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// estWidth approximates text width in px at the badge's 11px font (roughly 7px
// per character plus padding) so the two panels size to their content.
func estWidth(s string) int {
	return len(s)*7 + 10
}

// render produces a flat Shields-style SVG with a label panel and a value
// panel. Text is truncated, then XML-escaped.
//
// Both bounds live here rather than at the call sites so every current and
// future caller is covered by construction: Status, Uptime and the status-page
// badge all funnel through this one function.
func render(label, value, color string) string {
	label = escape(truncate(label))
	value = escape(truncate(value))
	lw := estWidth(label)
	vw := estWidth(value)
	total := lw + vw
	lx := lw * 10 / 2
	vx := (lw + vw/2) * 10

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`, total, label, value)
	fmt.Fprintf(&b, `<title>%s: %s</title>`, label, value)
	b.WriteString(`<linearGradient id="s" x2="0" y2="100%">`)
	b.WriteString(`<stop offset="0" stop-color="#bbb" stop-opacity=".1"/>`)
	b.WriteString(`<stop offset="1" stop-opacity=".1"/></linearGradient>`)
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`, total)
	b.WriteString(`<g clip-path="url(#r)">`)
	fmt.Fprintf(&b, `<rect width="%d" height="20" fill="%s"/>`, lw, labelBg)
	fmt.Fprintf(&b, `<rect x="%d" width="%d" height="20" fill="%s"/>`, lw, vw, color)
	fmt.Fprintf(&b, `<rect width="%d" height="20" fill="url(#s)"/></g>`, total)
	b.WriteString(`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="110" text-rendering="geometricPrecision">`)
	fmt.Fprintf(&b, `<text aria-hidden="true" x="%d" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="%d">%s</text>`, lx, (lw-10)*10, label)
	fmt.Fprintf(&b, `<text x="%d" y="140" transform="scale(.1)" textLength="%d">%s</text>`, lx, (lw-10)*10, label)
	fmt.Fprintf(&b, `<text aria-hidden="true" x="%d" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="%d">%s</text>`, vx, (vw-10)*10, value)
	fmt.Fprintf(&b, `<text x="%d" y="140" transform="scale(.1)" textLength="%d">%s</text>`, vx, (vw-10)*10, value)
	b.WriteString(`</g></svg>`)
	return b.String()
}

// escape replaces the XML-significant characters in badge text.
func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
