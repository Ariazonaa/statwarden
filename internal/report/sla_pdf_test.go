package report

import (
	"bytes"
	"testing"
	"time"
)

// TestWriteSLAPDF asserts WriteSLAPDF emits a non-empty, structurally valid PDF
// for a representative range: the %PDF magic header, an EOF trailer marker, and
// the xref/trailer keywords every conforming PDF carries.
func TestWriteSLAPDF(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rng := Range{From: from, To: from.Add(30 * 24 * time.Hour)}
	rows := []SLARow{
		{
			MonitorID:   1,
			MonitorName: "API (Frankfurt)",
			Uptime:      UptimeResult{Up: 990, Down: 10, Total: 1000, Uptime: 0.99},
			Percentiles: Percentiles{P50: 40, P95: 120, P99: 300, Avg: 55},
		},
		{
			MonitorID:   2,
			MonitorName: "Datenbank",
			Uptime:      UptimeResult{Up: 0, Down: 0, Total: 0, Uptime: -1}, // no data path
			Percentiles: Percentiles{},
		},
	}

	var buf bytes.Buffer
	if err := WriteSLAPDF(&buf, rng, rows); err != nil {
		t.Fatalf("WriteSLAPDF: %v", err)
	}

	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatal("WriteSLAPDF produced no bytes")
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatalf("missing %%PDF magic header; first bytes: %q", firstBytes(out, 8))
	}
	if !bytes.Contains(out, []byte("%%EOF")) {
		t.Fatal("missing EOF trailer marker")
	}
	for _, kw := range []string{"trailer", "xref", "/Root"} {
		if !bytes.Contains(out, []byte(kw)) {
			t.Fatalf("well-formed PDF should contain %q", kw)
		}
	}
}

// TestWriteSLAPDFEmpty guarantees a report with no monitors still yields a
// valid PDF (title/period/summary only) rather than erroring or empty output.
func TestWriteSLAPDFEmpty(t *testing.T) {
	var buf bytes.Buffer
	rng := Range{From: base, To: base.Add(time.Hour)}
	if err := WriteSLAPDF(&buf, rng, nil); err != nil {
		t.Fatalf("WriteSLAPDF empty: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
		t.Fatal("empty report is not a valid PDF")
	}
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
