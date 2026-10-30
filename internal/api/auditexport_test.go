package api

import (
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/siem"
)

// exportCSV downloads the audit log and parses it, failing the test on a
// non-200 or an unparseable body.
func exportCSV(t *testing.T, h *harness, target string, sess *session) ([][]string, *http.Response) {
	t.Helper()
	rec := h.requestWith(http.MethodGet, target, nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, body %s", target, rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v\n%s", err, rec.Body.String())
	}
	return rows, rec.Result()
}

func TestAuditExportCSVIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	viewer := h.createUser(admin, "viewer@example.com", "viewer")

	if rec := h.requestWith(http.MethodGet, "/api/v1/audit-log.csv", nil, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer got %d, want 403 — the audit log names who did what from which address", rec.Code)
	}
	if rec := h.request(http.MethodGet, "/api/v1/audit-log.csv", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous got %d, want 401", rec.Code)
	}
}

func TestAuditExportCSVHasHeaderAndRows(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	// Any mutation writes an audit row; a tag is the cheapest one.
	if rec := h.requestWith(http.MethodPost, "/api/v1/tags", map[string]any{"name": "prod", "color": "#ff0000"}, admin); rec.Code != http.StatusCreated {
		t.Fatalf("create tag = %d, body %s", rec.Code, rec.Body.String())
	}

	rows, resp := exportCSV(t, h, "/api/v1/audit-log.csv", admin)
	if got := resp.Header.Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment so the browser saves it", got)
	}
	if resp.Header.Get(auditExportTruncatedHeader) != "" {
		t.Errorf("a two-row export must not be flagged truncated")
	}

	want := []string{"id", "created_at", "user_id", "action", "entity_type", "entity_id", "ip", "details"}
	if len(rows) == 0 {
		t.Fatal("no rows at all, not even the header")
	}
	if strings.Join(rows[0], ",") != strings.Join(want, ",") {
		t.Fatalf("header = %v, want %v", rows[0], want)
	}
	var actions []string
	for _, r := range rows[1:] {
		actions = append(actions, r[3])
	}
	if !containsString(actions, "tag.create") {
		t.Errorf("actions = %v, want the tag.create that was just performed", actions)
	}
	// The download is itself recorded, but only AFTER the rows were read — so it
	// cannot appear in its own body, and it must appear in the next one.
	if containsString(actions, "audit.export") {
		t.Errorf("the export recorded itself into its own body: %v", actions)
	}
	rows2, _ := exportCSV(t, h, "/api/v1/audit-log.csv", admin)
	var actions2 []string
	for _, r := range rows2[1:] {
		actions2 = append(actions2, r[3])
	}
	if !containsString(actions2, "audit.export") {
		t.Errorf("exporting the audit log was not audited: %v", actions2)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// A monitor name reaches the export through the details blob, and the file is
// opened in a spreadsheet. A cell starting with = is a formula there, so the
// escape has to survive the whole path from the audit row to the CSV cell.
func TestAuditExportNeutralisesSpreadsheetFormulas(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	// Written straight to the store: no API surface accepts an action name, and
	// the point of this test is the CSV writer, not the audit call sites.
	if _, err := h.store.InsertAudit(context.Background(), AuditEntry{
		Action:     `=HYPERLINK("https://attacker.test")`,
		EntityType: "monitor",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}

	rows, _ := exportCSV(t, h, "/api/v1/audit-log.csv", admin)
	var found bool
	for _, r := range rows[1:] {
		if strings.Contains(r[3], "HYPERLINK") {
			found = true
			if !strings.HasPrefix(r[3], "'") {
				t.Errorf("formula cell %q is not neutralised; it will execute on open", r[3])
			}
		}
	}
	if !found {
		t.Fatal("the seeded row never reached the export")
	}
}

func TestAuditExportWindowFilters(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	old := time.Now().Add(-72 * time.Hour)
	if _, err := h.store.InsertAudit(context.Background(), AuditEntry{
		Action: "ancient.event", CreatedAt: old,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rows, _ := exportCSV(t, h, "/api/v1/audit-log.csv?from="+from, admin)
	for _, r := range rows[1:] {
		if r[3] == "ancient.event" {
			t.Fatalf("an entry from before ?from= was exported: %v", r)
		}
	}
	// Without the window it is there, so the filter is what excluded it and not
	// a seeding failure.
	all, _ := exportCSV(t, h, "/api/v1/audit-log.csv", admin)
	var seen bool
	for _, r := range all[1:] {
		if r[3] == "ancient.event" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the seeded old entry is missing from the unfiltered export too")
	}
}

// The export is a file someone files as evidence, so its order has to be the
// one the column says — not the order rows happened to be inserted in. An entry
// can carry an explicit CreatedAt (the pruner backdates, imports seed history),
// and MemStore is only a useful double if it answers that the way Postgres does.
func TestAuditExportIsOrderedNewestFirst(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	base := time.Now().Add(-24 * time.Hour)
	// Inserted oldest-last on purpose: insertion order is the WRONG answer here.
	for _, offset := range []time.Duration{2 * time.Hour, 0, time.Hour} {
		if _, err := h.store.InsertAudit(context.Background(), AuditEntry{
			Action:    "seed." + strings.ReplaceAll(offset.String(), "0s", "now"),
			CreatedAt: base.Add(offset),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rows, _ := exportCSV(t, h, "/api/v1/audit-log.csv", admin)
	var stamps []time.Time
	for _, r := range rows[1:] {
		ts, err := time.Parse(time.RFC3339, r[1])
		if err != nil {
			t.Fatalf("unparseable created_at %q: %v", r[1], err)
		}
		stamps = append(stamps, ts)
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i].After(stamps[i-1]) {
			t.Fatalf("row %d (%s) is newer than the row above it (%s); the export is not newest-first",
				i, stamps[i], stamps[i-1])
		}
	}
}

func TestAuditExportRejectsAnUnparseableWindow(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	for _, target := range []string{
		"/api/v1/audit-log.csv?from=yesterday",
		"/api/v1/audit-log.csv?to=soon",
		// to before from describes no window at all.
		"/api/v1/audit-log.csv?from=2025-06-01T00:00:00Z&to=2025-01-01T00:00:00Z",
	} {
		rec := h.requestWith(http.MethodGet, target, nil, admin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 — a guessed window turns evidence into fiction", target, rec.Code)
		}
	}
}

// recordingSink captures what the API hands the SIEM feed.
type recordingSink struct {
	events []siem.Event
}

func (r *recordingSink) HandleAudit(_ context.Context, e siem.Event) {
	r.events = append(r.events, e)
}

func TestAuditEventsReachTheSIEMSink(t *testing.T) {
	sink := &recordingSink{}
	h := newHarnessTweaked(t, func(o *Options) { o.AuditSink = sink })
	admin := h.setupAdmin()
	if rec := h.requestWith(http.MethodPost, "/api/v1/tags", map[string]any{"name": "prod", "color": "#ff0000"}, admin); rec.Code != http.StatusCreated {
		t.Fatalf("create tag = %d", rec.Code)
	}

	var got *siem.Event
	for i := range sink.events {
		if sink.events[i].Action == "tag.create" {
			got = &sink.events[i]
		}
	}
	if got == nil {
		t.Fatalf("tag.create never reached the sink; saw %d events", len(sink.events))
	}
	// The id is what lets a collector deduplicate and join against the CSV
	// export. Before InsertAudit returned the stored row it was always 0.
	if got.ID == 0 {
		t.Error("forwarded event carries no audit row id")
	}
	if got.EntityType != "tag" {
		t.Errorf("entity_type = %q, want tag", got.EntityType)
	}
	if got.At.IsZero() {
		t.Error("forwarded event has no timestamp")
	}
}

// A store that cannot record the entry must not produce a forwarded event
// either: the local log is the record of truth, and a collector told about an
// event this instance has no row for cannot be reconciled with anything.
func TestAFailedAuditWriteIsNotForwarded(t *testing.T) {
	sink := &recordingSink{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &server{store: failingAuditStore{NewMemStore()}, auditSink: sink, logger: log, now: time.Now}
	s.audit(context.Background(), nil, "tag.create", "tag", nil, "", nil)
	if len(sink.events) != 0 {
		t.Fatalf("forwarded %d events after the write failed", len(sink.events))
	}
}

// failingAuditStore is a Store whose audit INSERT always fails.
type failingAuditStore struct{ *MemStore }

func (failingAuditStore) InsertAudit(context.Context, AuditEntry) (AuditEntry, error) {
	return AuditEntry{}, context.DeadlineExceeded
}

// A truncated export has to say so IN THE FILE.
//
// The response header is for API clients. The product's only caller is an
// <a download>, which navigates and saves — no script sees the headers, so the
// person who opens fifty thousand rows in a spreadsheet had no way to know the
// oldest ones were dropped. Nothing about the file looked partial.
func TestATruncatedExportSaysSoInTheFile(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	// The cap is 50k; writing that many rows through the API would take minutes.
	// The writer is called directly instead, which is the unit that owns the
	// marker.
	var buf strings.Builder
	entries := []AuditEntry{
		{ID: 2, CreatedAt: time.Now().UTC(), Action: "tag.create"},
		{ID: 1, CreatedAt: time.Now().UTC(), Action: "user.login"},
	}
	if err := writeAuditCSV(&buf, entries, true); err != nil {
		t.Fatalf("writeAuditCSV: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("a truncated export is not valid CSV: %v\n%s", err, buf.String())
	}
	// header + 2 entries + the marker.
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (header, two entries, marker):\n%s", len(rows), buf.String())
	}
	marker := rows[len(rows)-1]
	if len(marker) != len(rows[0]) {
		t.Errorf("the marker row has %d cells, the header has %d; it must stay well-formed CSV",
			len(marker), len(rows[0]))
	}
	if marker[3] != auditExportTruncatedAction {
		t.Errorf("marker action = %q, want %q", marker[3], auditExportTruncatedAction)
	}
	if marker[0] != "" {
		t.Errorf("the marker carries id %q; an empty id is what keeps it from looking like an entry", marker[0])
	}
	if !strings.Contains(marker[7], "OLDEST") {
		t.Errorf("the marker does not say WHICH half is missing: %q", marker[7])
	}

	// And a complete export carries no marker at all.
	var full strings.Builder
	if err := writeAuditCSV(&full, entries, false); err != nil {
		t.Fatalf("writeAuditCSV: %v", err)
	}
	if strings.Contains(full.String(), auditExportTruncatedAction) {
		t.Errorf("a complete export claims to be truncated:\n%s", full.String())
	}

	// The header is still set for API clients, on the real endpoint.
	_, resp := exportCSV(t, h, "/api/v1/audit-log.csv", admin)
	if resp.Header.Get(auditExportTruncatedHeader) != "" {
		t.Error("a small export is flagged truncated")
	}
}

// countingAuditStore records the limit the handler asks for and answers with a
// scripted number of rows.
type countingAuditStore struct {
	*MemStore
	askedLimit int
	rows       int
}

func (c *countingAuditStore) ExportAudit(ctx context.Context, from, to time.Time, limit int) ([]AuditEntry, error) {
	c.askedLimit = limit
	out := make([]AuditEntry, 0, c.rows)
	for i := range c.rows {
		out = append(out, AuditEntry{
			ID: int64(i + 1), CreatedAt: time.Now().UTC(), Action: "tag.create",
		})
	}
	return out, nil
}

// The handler's truncation DETECTION, which nothing exercised.
//
// TestATruncatedExportSaysSoInTheFile proves the writer emits a marker when it
// is handed truncated=true; every HTTP-level assertion checks the header is
// ABSENT on a small export. So inverting `len(entries) > maxAuditExportRows`, or
// dropping the `+1` from the probe, would have left every assertion in the file
// holding while neither signal was ever produced again.
func TestTheHandlerDetectsTruncation(t *testing.T) {
	store := &countingAuditStore{MemStore: NewMemStore()}
	h := newHarnessTweaked(t, func(o *Options) { o.Store = store })
	admin := h.setupAdmin()

	// One more row than the cap is what proves there was more; asking for
	// exactly the cap cannot distinguish "full" from "overflowing".
	store.rows = maxAuditExportRows + 1
	rec := h.requestWith(http.MethodGet, "/api/v1/audit-log.csv", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	if store.askedLimit != maxAuditExportRows+1 {
		t.Errorf("the handler asked for %d rows, want %d: it cannot tell a full window "+
			"from an overflowing one", store.askedLimit, maxAuditExportRows+1)
	}
	if rec.Result().Header.Get(auditExportTruncatedHeader) != "true" {
		t.Error("an overflowing export is not flagged truncated in the header")
	}
	if !strings.Contains(rec.Body.String(), auditExportTruncatedAction) {
		t.Error("an overflowing export carries no marker row in the body")
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the truncated export is not valid CSV: %v", err)
	}
	// header + the cap + the marker.
	if len(rows) != maxAuditExportRows+2 {
		t.Errorf("rows = %d, want %d (header, %d entries, marker)",
			len(rows), maxAuditExportRows+2, maxAuditExportRows)
	}

	// Exactly the cap is NOT truncated: there was no more to fetch.
	store.rows = maxAuditExportRows
	rec = h.requestWith(http.MethodGet, "/api/v1/audit-log.csv", nil, admin)
	if rec.Result().Header.Get(auditExportTruncatedHeader) != "" {
		t.Error("an export that exactly fills the window is flagged truncated")
	}
	if strings.Contains(rec.Body.String(), auditExportTruncatedAction) {
		t.Error("an export that exactly fills the window carries a marker row")
	}
}
