package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
)

// writeJSON prints v as indented JSON. This is the scripting escape hatch: it
// re-encodes the decoded view, so anything the table columns do not show is
// still reachable without waiting for the CLI to catch up with the API.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeMonitorTable renders a monitor list as an aligned table.
func writeMonitorTable(w io.Writer, monitors []MonitorView) error {
	if len(monitors) == 0 {
		_, err := fmt.Fprintln(w, "No monitors.")
		return err
	}
	// Writes go into the tabwriter's buffer; the only error that matters
	// surfaces at Flush, which is returned.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tUPTIME 24H\tLATENCY\tURL")
	for _, m := range monitors {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID,
			truncate(terminalSafe(m.Name), 32),
			terminalSafe(m.Type),
			terminalSafe(statusLabel(m)),
			formatUptime(m.Uptime24h),
			formatLatency(m.LatencyMs),
			truncate(terminalSafe(m.URL), 44),
		)
	}
	return tw.Flush()
}

// writeMonitorDetail renders a single monitor as a key/value block.
func writeMonitorDetail(w io.Writer, m MonitorView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k string, v string) { _, _ = fmt.Fprintf(tw, "%s\t%s\n", k, terminalSafe(v)) }

	row("ID", strconv.FormatInt(m.ID, 10))
	row("Name", m.Name)
	row("Type", m.Type)
	row("Status", statusLabel(m))
	row("URL", m.URL)
	row("Interval", fmt.Sprintf("%ds", m.Interval))
	if m.Timeout != nil {
		row("Timeout", fmt.Sprintf("%ds", *m.Timeout))
	}
	row("Max retries", strconv.Itoa(m.MaxRetries))
	row("Retry interval", fmt.Sprintf("%ds", m.RetryInterval))
	row("Upside down", strconv.FormatBool(m.UpsideDown))
	if m.Description != "" {
		row("Description", m.Description)
	}
	if m.LinkURL != "" {
		row("Link", m.LinkURL)
	}
	row("Latency", formatLatency(m.LatencyMs))
	row("Uptime 24h", formatUptime(m.Uptime24h))
	row("Uptime 30d", formatUptime(m.Uptime30d))
	row("Uptime 1y", formatUptime(m.Uptime1y))
	if m.LastCheckedAt != nil {
		row("Last checked", m.LastCheckedAt.Local().Format("2006-01-02 15:04:05"))
	}
	row("Created", m.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	if len(m.Config) > 0 {
		row("Config", compactJSON(m.Config))
	}
	return tw.Flush()
}

// statusLabel folds the active flag into the status column: a paused monitor's
// last known status is stale and reporting it as "up" would be a lie.
func statusLabel(m MonitorView) string {
	if !m.Active {
		return "paused"
	}
	if m.Status == "" {
		return "unknown"
	}
	return m.Status
}

func formatUptime(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", *v)
}

func formatLatency(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v) + "ms"
}

// terminalSafe replaces every control character and every Unicode bidi
// formatting character in s with U+FFFD before it is printed.
//
// Monitor names, URLs, descriptions and error details come from the server,
// and any user with the writer role sets them. Printed raw, an escape sequence
// in a monitor name drove the terminal of whoever listed monitors — retitled
// it, cleared it, rewrote earlier lines — a newline or tab forged rows and
// columns, and a bidi override made text read differently from what it is.
// Only the table and detail renderers need this; --json output is escaped by
// the encoder already.
func terminalSafe(s string) string {
	if strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return '�'
		}
		return r
	}, s)
}

// unsafeRune reports C0/C1 control characters, DEL, and the bidi embedding,
// override and isolate controls.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) ||
		(r >= '\u202a' && r <= '\u202e') ||
		(r >= '\u2066' && r <= '\u2069') ||
		r == '\u200e' || r == '\u200f' || r == '\u061c'
}

func truncate(s string, max int) string {
	if max <= 1 || len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// compactJSON renders raw JSON on one line so it fits a table row, falling back
// to the original bytes if it is not valid JSON.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
