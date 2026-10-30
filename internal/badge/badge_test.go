package badge

import (
	"encoding/xml"
	"strings"
	"testing"
	"unicode/utf8"
)

// wellFormed reports whether s is parseable XML (a decent SVG sanity check).
func wellFormed(t *testing.T, s string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("SVG not well-formed: %v\n%s", err, s)
		}
	}
}

func TestStatusBadge(t *testing.T) {
	tests := []struct {
		status string
		color  string
	}{
		{"up", colorGreen},
		{"down", colorRed},
		{"pending", colorYellow},
		{"maintenance", colorBlue},
		{"paused", colorGray},
		{"weird", colorGray},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			svg := Status("status", tt.status)
			wellFormed(t, svg)
			if !strings.Contains(svg, tt.color) {
				t.Fatalf("status %q badge missing color %q", tt.status, tt.color)
			}
			if !strings.Contains(svg, ">"+tt.status+"<") {
				t.Fatalf("status %q badge missing value text", tt.status)
			}
		})
	}
}

func TestUptimeBadge(t *testing.T) {
	tests := []struct {
		pct   float64
		color string
		text  string
	}{
		{100, colorGreen, "100.00%"},
		{99.5, colorGreen, "99.50%"},
		{96, colorGreenYellow, "96.00%"},
		{92, colorYellow, "92.00%"},
		{80, colorOrange, "80.00%"},
		{50, colorRed, "50.00%"},
		{-1, colorGray, "n/a"},
	}
	for _, tt := range tests {
		svg := Uptime("uptime", tt.pct)
		wellFormed(t, svg)
		if !strings.Contains(svg, tt.color) {
			t.Fatalf("uptime %.1f badge missing color %q", tt.pct, tt.color)
		}
		if !strings.Contains(svg, tt.text) {
			t.Fatalf("uptime %.1f badge missing text %q:\n%s", tt.pct, tt.text, svg)
		}
	}
}

func TestEscape(t *testing.T) {
	svg := Status("a&b", "up")
	wellFormed(t, svg)
	if !strings.Contains(svg, "a&amp;b") {
		t.Fatalf("label not escaped: %s", svg)
	}
}

// TestBadgeLabelIsBounded is the regression test for the amplification: ?label=
// used to reach render unbounded, so a query string near Go's 1 MiB header
// limit produced a multi-MiB SVG on a public, widely embedded endpoint.
func TestBadgeLabelIsBounded(t *testing.T) {
	// The worst case: every character escapes to five bytes, and each panel is
	// written four times.
	huge := strings.Repeat("&", 200_000)

	svg := Status(huge, "up")
	wellFormed(t, svg)
	if len(svg) > 4096 {
		t.Fatalf("badge for a %d-char label rendered %d bytes, want a bounded response", len(huge), len(svg))
	}

	// The value panel is bounded by the same construction, so a future caller
	// passing something unbounded there is covered too.
	svg = Status("status", huge)
	wellFormed(t, svg)
	if len(svg) > 4096 {
		t.Fatalf("badge for a %d-char value rendered %d bytes, want a bounded response", len(huge), len(svg))
	}
}

// TestTruncateCutsOnRunes pins that the cut lands on a character boundary. A
// byte-wise cut would emit invalid UTF-8 into the SVG, and truncating after
// escaping could split an entity and leave a bare "&am".
func TestTruncateCutsOnRunes(t *testing.T) {
	short := "übersicht"
	if got := truncate(short); got != short {
		t.Fatalf("truncate(%q) = %q, want it unchanged", short, got)
	}

	long := strings.Repeat("ü", maxPanelRunes*2)
	got := truncate(long)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate cut mid-rune: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxPanelRunes+1 { // +1 for the ellipsis
		t.Fatalf("truncated to %d runes, want %d", n, maxPanelRunes+1)
	}

	// An entity-heavy label survives escaping intact.
	svg := Status(strings.Repeat("&", maxPanelRunes*2), "up")
	wellFormed(t, svg)
	if strings.Contains(svg, "&am<") || strings.Contains(svg, "&amp<") {
		t.Fatalf("escaped entity was cut in half:\n%s", svg)
	}
}

// The uptime badge TRUNCATES, so a period with downtime in it can never render
// as perfect.
//
// FormatFloat rounds, so 99.995 became "100.00%" — on the number people paste
// into a README. The availability archive already truncated for exactly this
// reason; the badge disagreed with it. Every case in the existing table is
// already exact at two decimals, so none of them could see this.
func TestUptimeBadgeTruncatesRatherThanRounds(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{99.995, "99.99%"}, // the one that used to read 100.00%
		{99.999, "99.99%"},
		{100, "100.00%"}, // a genuinely perfect period still says so
		{99.994, "99.99%"},
		{0, "0.00%"},
	}
	for _, c := range cases {
		got := Uptime("uptime", c.pct)
		if !strings.Contains(got, c.want) {
			t.Errorf("Uptime(%v) does not render %q:\n%s", c.pct, c.want, got)
		}
		if c.pct < 100 && strings.Contains(got, ">100.00%<") {
			t.Errorf("Uptime(%v) rendered a perfect period for one that was not", c.pct)
		}
	}
}
