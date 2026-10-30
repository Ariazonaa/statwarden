package api

import (
	"bytes"
	"net/http"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// Report window bounds.
const (
	// maxReportSpan caps an explicit ?from=/?to= window.
	//
	// Every report endpoint scans heartbeats, and Flakiest and the SLA report do
	// it once PER MONITOR, so an unbounded window let any authenticated user turn
	// a single request into a full-table scan. Service.Uptime also walks the
	// range a day at a time looking for rollup gaps, so a from= in the year 1000
	// is a million-iteration loop on top of that. A little over a year covers
	// every legitimate annual report.
	maxReportSpan = 400 * 24 * time.Hour
	// maxFlakiestLimit caps the flakiest ranking; the parameter had no ceiling.
	maxFlakiestLimit = 500
	// slaPageSize is how many monitors one SLA read pulls. The report pages
	// through all of them rather than stopping at a silent cap.
	slaPageSize = 500
)

// parseReportRange reads the requested window from the query string. It accepts
// a named ?range= (24h, 7d, 30d, 1y) or an explicit ?from=&to= pair (RFC3339).
// The default is the last 30 days.
//
// An explicit window is validated rather than patched up. It used to be taken
// verbatim: unparseable bounds fell through to the default, from could sit
// after to, and to could be in the future — which makes DowntimeLog report an
// ongoing outage as running into next year. Guessing at any of those hides a
// caller's bug behind plausible-looking numbers, so each is a 400.
func (s *server) parseReportRange(r *http.Request) (report.Range, error) {
	now := s.now()
	q := r.URL.Query()
	if from := q.Get("from"); from != "" {
		fromT, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return report.Range{}, validationError("from must be an RFC3339 timestamp")
		}
		toT := now
		if to := q.Get("to"); to != "" {
			parsed, perr := time.Parse(time.RFC3339, to)
			if perr != nil {
				return report.Range{}, validationError("to must be an RFC3339 timestamp")
			}
			toT = parsed
		}
		// A window may not run past now: there is no data there, and an outage
		// still open at the end of the window is reported as lasting until it.
		if toT.After(now) {
			toT = now
		}
		if !toT.After(fromT) {
			return report.Range{}, validationError("from must be before to")
		}
		if toT.Sub(fromT) > maxReportSpan {
			return report.Range{}, validationError("the requested window is too large")
		}
		return report.Range{From: fromT, To: toT}, nil
	}
	var d time.Duration
	switch q.Get("range") {
	case "24h", "1d":
		d = 24 * time.Hour
	case "7d":
		d = 7 * 24 * time.Hour
	case "1y":
		d = 365 * 24 * time.Hour
	case "30d", "":
		d = 30 * 24 * time.Hour
	default:
		d = 30 * 24 * time.Hour
	}
	return report.Range{From: now.Add(-d), To: now}, nil
}

func (s *server) handleMonitorUptime(w http.ResponseWriter, r *http.Request) {
	if s.reporter == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "reporting not available")
		return
	}
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	rng, rerr := s.parseReportRange(r)
	if rerr != nil {
		writeProblemDetail(w, http.StatusBadRequest, rerr.Error())
		return
	}
	res, err := s.reporter.Uptime(r.Context(), id, rng)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "compute uptime")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleMonitorReport(w http.ResponseWriter, r *http.Request) {
	if s.reporter == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "reporting not available")
		return
	}
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); storeError(w, err) {
		return
	}
	rng, rerr := s.parseReportRange(r)
	if rerr != nil {
		writeProblemDetail(w, http.StatusBadRequest, rerr.Error())
		return
	}
	rep, err := s.reporter.Report(r.Context(), id, rng)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "compute report")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *server) handleFlakiest(w http.ResponseWriter, r *http.Request) {
	if s.reporter == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "reporting not available")
		return
	}
	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			// Capped: Flakiest scans heartbeats once per monitor, so an
			// unbounded limit is another way to ask for a full-table scan.
			limit = min(n, maxFlakiestLimit)
		}
	}
	rng, rerr := s.parseReportRange(r)
	if rerr != nil {
		writeProblemDetail(w, http.StatusBadRequest, rerr.Error())
		return
	}
	items, err := s.reporter.Flakiest(r.Context(), rng, limit)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "rank flakiest")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) handleCertOverview(w http.ResponseWriter, r *http.Request) {
	if s.reporter == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "reporting not available")
		return
	}
	rows, err := s.reporter.CertOverview(r.Context())
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "cert overview")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// slaRows builds an SLA row per monitor for the given range.
//
// It pages through every monitor. The single capped read it replaced stopped at
// 1 000 and said nothing, so a large instance produced an SLA report that
// silently omitted monitors — in a document whose whole purpose is to be
// complete.
func (s *server) slaRows(r *http.Request, rng report.Range) ([]report.SLARow, error) {
	var monitors []domain.Monitor
	for offset := 0; ; offset += slaPageSize {
		page, total, err := s.store.ListMonitors(r.Context(), MonitorFilter{}, Page{Limit: slaPageSize, Offset: offset})
		if err != nil {
			return nil, err
		}
		monitors = append(monitors, page...)
		if len(page) == 0 || len(monitors) >= total {
			break
		}
	}
	rows := make([]report.SLARow, 0, len(monitors))
	for _, m := range monitors {
		rep, err := s.reporter.Report(r.Context(), m.ID, rng)
		if err != nil {
			return nil, err
		}
		rows = append(rows, report.SLARow{
			MonitorID:   m.ID,
			MonitorName: m.Name,
			Uptime:      rep.Uptime,
			Percentiles: rep.Percentiles,
		})
	}
	return rows, nil
}

func (s *server) handleSLACSV(w http.ResponseWriter, r *http.Request) {
	if s.reporter == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "reporting not available")
		return
	}
	rng, rerr := s.parseReportRange(r)
	if rerr != nil {
		writeProblemDetail(w, http.StatusBadRequest, rerr.Error())
		return
	}
	rows, err := s.slaRows(r, rng)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "build SLA report")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="statwarden-sla.csv"`)
	if err := report.WriteSLACSV(w, rng, rows); err != nil {
		s.logger.Error("write SLA CSV", "error", err)
	}
}

// handleSLAPDF streams the SLA report as a print-ready PDF. The document is
// rendered fully in memory first so a mid-render failure can still surface as a
// problem+json response rather than a truncated body with 200 already sent.
func (s *server) handleSLAPDF(w http.ResponseWriter, r *http.Request) {
	if s.reporter == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "reporting not available")
		return
	}
	rng, rerr := s.parseReportRange(r)
	if rerr != nil {
		writeProblemDetail(w, http.StatusBadRequest, rerr.Error())
		return
	}
	rows, err := s.slaRows(r, rng)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "build SLA report")
		return
	}
	var buf bytes.Buffer
	if err := report.WriteSLAPDF(&buf, rng, rows); err != nil {
		s.logger.Error("write SLA PDF", "error", err)
		writeProblemDetail(w, http.StatusInternalServerError, "render SLA PDF")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="statwarden-sla.pdf"`)
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.logger.Error("stream SLA PDF", "error", err)
	}
}
