package api

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/report"
)

// Audit-log export bounds.
const (
	// maxAuditExportRows caps one CSV download.
	//
	// The export is an admin-only full-table read of a table that grows with
	// every mutating request in the product, so it needs a ceiling or a
	// long-lived instance turns one click into a multi-hundred-megabyte
	// response streamed out of a single unbounded query. The cap is high enough
	// that a normal instance exports its whole history in one file and low
	// enough that a busy one is told to narrow the window instead of being
	// handed a truncated file that looks complete.
	maxAuditExportRows = 50000

	// auditExportTruncatedHeader tells an API CLIENT that the window did not fit.
	//
	// It is not the only signal, and it was never sufficient on its own. The
	// product's own caller is an <a download>: the browser navigates and saves
	// the file, so no script ever sees this header. writeAuditCSV therefore also
	// appends a marker row to the body — a full-width, well-formed row with an
	// empty id, which is what the person who opens the spreadsheet actually
	// finds. This header stays for callers that can read it.
	auditExportTruncatedHeader = "X-Statwarden-Export-Truncated"
)

// parseAuditWindow reads the export window from the query string: an optional
// RFC3339 ?from= and ?to=, defaulting to "everything up to now".
//
// It deliberately does NOT reuse parseReportRange. That one defaults to the last
// 30 days and refuses a span over ~400 days, which is right for a report that
// scans heartbeats per monitor and wrong here: an audit export exists to answer
// "show me everything", and silently handing back one month of a three-year log
// is the failure mode this endpoint must not have. The row cap, not the span,
// is what bounds the query.
//
// An unparseable bound is a 400 rather than a fallback to the default, for the
// reason parseReportRange gives: guessing hides the caller's bug behind
// plausible-looking output, and here that output is evidence.
func (s *server) parseAuditWindow(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	to = s.now()
	if raw := q.Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, validationError("from must be an RFC3339 timestamp")
		}
	}
	if raw := q.Get("to"); raw != "" {
		parsed, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			return time.Time{}, time.Time{}, validationError("to must be an RFC3339 timestamp")
		}
		to = parsed
	}
	// Both bounds are inclusive and the zero `from` means "since the beginning",
	// so the only ordering that cannot describe a window is to-before-from.
	if !from.IsZero() && to.Before(from) {
		return time.Time{}, time.Time{}, validationError("from must be before to")
	}
	return from, to, nil
}

// handleExportAuditCSV streams the audit log as CSV (admin only).
//
// It is the pull half of the audit-log export; internal/siem is the push half.
// Both read the same rows, and an operator assembling evidence after the fact
// wants this one — a file with the whole window in it, not a replay of a feed
// that may have dropped events while a collector was down.
func (s *server) handleExportAuditCSV(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.parseAuditWindow(r)
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	// One row more than will be emitted: that extra row is how truncation is
	// detected without a second count query, and it is dropped before writing.
	entries, err := s.store.ExportAudit(r.Context(), from, to, maxAuditExportRows+1)
	if storeError(w, err) {
		return
	}
	// The store answers newest first, so the rows kept are the newest ones and
	// the OLDEST are dropped. That is the right half — an operator who hit the
	// cap narrows the window with ?from= and takes the older slice in a second
	// file — but it is only right because the caller is told the file is partial.
	truncated := len(entries) > maxAuditExportRows
	if truncated {
		entries = entries[:maxAuditExportRows]
	}

	// Exporting the audit log is itself an auditable act — it is the one
	// operation that carries every other operation out of the instance — so it
	// is recorded before the body is written, while a failure can still be seen.
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "audit.export", "audit_log", nil, clientIP(r), map[string]any{
		"rows":      len(entries),
		"truncated": truncated,
	})

	// Headers before the first byte of the body: once WriteHeader has run,
	// nothing here can be added, and the truncation flag is the part the caller
	// must not miss.
	if truncated {
		w.Header().Set(auditExportTruncatedHeader, "true")
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="statwarden-audit-log.csv"`)
	// The header is for API clients. The BODY is for the person who downloads
	// it — and that person is the one this matters to.
	//
	// The browser reaches this endpoint through an <a download>, which navigates
	// and saves; no script ever sees the response headers, so the flag above was
	// invisible to the only caller the product actually has. Someone opened a
	// file, saw fifty thousand rows, and had no way to know the oldest ones had
	// been dropped. The marker goes in the file itself, as a well-formed final
	// row, where whoever opens the spreadsheet finds it.
	if err := writeAuditCSV(w, entries, truncated); err != nil {
		// The status is already 200 and rows are already on the wire, so this
		// cannot become a problem+json. Log it: a truncated file that the
		// operator believes is complete is exactly what must not pass silently.
		s.logger.Error("write audit CSV", "error", err)
	}
}

// auditExportTruncatedAction is the action cell of the marker row appended to a
// truncated export. It is a fixed string so a script can grep for it.
const auditExportTruncatedAction = "statwarden.export_truncated"

// writeAuditCSV writes entries as CSV, newest first, followed by a marker row
// when the export hit the row cap.
//
// Every free-text cell goes through report.CSVSafe. The audit log is full of
// operator-supplied strings — a monitor name in `details`, an action on an
// entity someone named — and this file is opened in Excel, where a cell starting
// with = is a formula, not text. See that function for the whole story.
func writeAuditCSV(w io.Writer, entries []AuditEntry, truncated bool) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"id", "created_at", "user_id", "action", "entity_type", "entity_id", "ip", "details",
	}); err != nil {
		return err
	}
	for _, e := range entries {
		rec := []string{
			strconv.FormatInt(e.ID, 10),
			e.CreatedAt.UTC().Format(time.RFC3339),
			optInt64(e.UserID),
			report.CSVSafe(e.Action),
			report.CSVSafe(e.EntityType),
			optInt64(e.EntityID),
			report.CSVSafe(e.IP),
			report.CSVSafe(string(e.Details)),
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	if truncated {
		// A full-width row, so the file stays valid CSV for anything that parses
		// it, and legible for the person who opens it in a spreadsheet. It cannot
		// collide with a real entry: the id cell is empty, and every real row has
		// one.
		if err := cw.Write([]string{
			// EMPTY created_at, not time.Now().
			//
			// The file is documented and tested as newest-first, and every real
			// row carries a timestamp — so stamping the marker with "now" made it
			// strictly newer than everything above it. Sorted by date in a
			// spreadsheet it jumped to the top as the most recent audit action,
			// and a script following the marker's own advice ("narrow it with
			// ?from=") read it as the oldest-seen cursor, got `now`, re-requested
			// the identical window and looped. An empty cell sorts out of the way
			// and cannot be mistaken for an event that happened.
			"", "", "",
			auditExportTruncatedAction, "", "", "",
			fmt.Sprintf("this export stopped at the %d-row limit; the OLDEST entries in the "+
				"requested window are missing. Narrow it with ?from=/?to= and download the "+
				"remainder separately.", maxAuditExportRows),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// optInt64 renders a nullable id as a decimal, or an empty cell when absent. An
// absent actor (a failed login) must not read as user 0.
func optInt64(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}
