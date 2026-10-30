//go:build integration

// Live-PostgreSQL tests for the PgStore status-page methods. See
// pgstore_integration_test.go for how the database is provisioned.
//
// The round-trip tests below give every text column a value that is unique
// across the whole row. That is the point: a swapped placeholder — writing the
// footer into `description`, or shifting every argument by one after a
// renumbering — is not a compile error and MemStore cannot see it, so only an
// assertion that each column holds its own distinct value catches it.
package api

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// testLogoPath is a path that survives sanitizeLogoPath, which rejects anything
// the server did not issue. A free-text value here would read back as "" and
// quietly mask a logo bug.
const testLogoPath = "/uploads/logos/1-0123456789abcdef0123456789abcdef.png"

const testLogoPath2 = "/uploads/logos/2-fedcba9876543210fedcba9876543210.webp"

// fullPage is a status page with every writable column set to a distinct,
// recognisable value.
func fullPage() StatusPage {
	return StatusPage{
		Slug:          "roundtrip-slug",
		Title:         "roundtrip-title",
		Description:   "roundtrip-description",
		FooterText:    "roundtrip-footer",
		Theme:         "dark",
		LogoPath:      testLogoPath,
		CustomDomain:  "roundtrip.example.test",
		Locale:        "de-DE",
		AccentColor:   "#1a2b3c",
		Published:     false,
		AllowIndexing: true,
		PasswordHash:  "argon2id$roundtrip",
	}
}

// assertPageColumns compares every column a caller can write, plus the derived
// HasPassword flag. Fields are checked individually so a failure names the
// column that moved rather than dumping two structs.
func assertPageColumns(t *testing.T, got, want StatusPage) {
	t.Helper()
	for _, c := range []struct {
		col       string
		got, want any
	}{
		{"slug", got.Slug, want.Slug},
		{"title", got.Title, want.Title},
		{"description", got.Description, want.Description},
		{"footer_text", got.FooterText, want.FooterText},
		{"theme", got.Theme, want.Theme},
		{"logo_path", got.LogoPath, want.LogoPath},
		{"custom_domain", got.CustomDomain, want.CustomDomain},
		{"locale", got.Locale, want.Locale},
		{"accent_color", got.AccentColor, want.AccentColor},
		{"published", got.Published, want.Published},
		{"allow_indexing", got.AllowIndexing, want.AllowIndexing},
		{"password_hash", got.PasswordHash, want.PasswordHash},
		{"has_password", got.HasPassword, want.PasswordHash != ""},
	} {
		if c.got != c.want {
			t.Errorf("%s = %#v, want %#v", c.col, c.got, c.want)
		}
	}
}

func TestPgCreateStatusPageWritesEveryColumn(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	in := fullPage()

	got, err := s.CreateStatusPage(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assertPageColumns(t, got, in)
	if got.ID == 0 {
		t.Errorf("id = 0, want a generated identity")
	}
	if got.ViewCount != 0 {
		t.Errorf("view_count = %d, want 0 on a fresh page", got.ViewCount)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("created_at = %v, updated_at = %v, want both set", got.CreatedAt, got.UpdatedAt)
	}

	// The same row read back through every reader must agree — each one selects
	// pageCols independently.
	byID, err := s.GetStatusPage(ctx, got.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	assertPageColumns(t, byID, in)

	bySlug, err := s.GetStatusPageBySlug(ctx, in.Slug)
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	assertPageColumns(t, bySlug, in)

	// Domain lookup is lower()-folded on both sides.
	byDomain, err := s.GetStatusPageByDomain(ctx, "ROUNDTRIP.Example.TEST")
	if err != nil {
		t.Fatalf("get by domain: %v", err)
	}
	assertPageColumns(t, byDomain, in)

	list, err := s.ListStatusPages(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list returned %d pages, want 1", len(list))
	}
	assertPageColumns(t, list[0], in)
}

// TestPgUpdateStatusPageWritesEveryColumn is the direct regression test for the
// §4 UpdateStatusPage placeholder renumbering: the SET clause numbers $2..$11
// against a hand-built args slice, so an off-by-one silently writes each value
// into its neighbour's column.
func TestPgUpdateStatusPageWritesEveryColumn(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	created, err := s.CreateStatusPage(ctx, fullPage())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Every value differs from the created one, and from every other column.
	upd := StatusPage{
		Slug:          "updated-slug",
		Title:         "updated-title",
		Description:   "updated-description",
		FooterText:    "updated-footer",
		Theme:         "light",
		CustomDomain:  "updated.example.test",
		Locale:        "fr-FR",
		AccentColor:   "#c3b2a1",
		Published:     true,
		AllowIndexing: false,
	}

	got, err := s.UpdateStatusPage(ctx, created.ID, upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	want := upd
	// An empty PasswordHash means "keep the existing one", and logo_path is
	// absent from the SET clause by design.
	want.PasswordHash = created.PasswordHash
	want.LogoPath = testLogoPath
	assertPageColumns(t, got, want)

	// Re-read: the assertion above could pass on a RETURNING clause that lied.
	reread, err := s.GetStatusPage(ctx, created.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	assertPageColumns(t, reread, want)

	if !reread.UpdatedAt.After(created.UpdatedAt) && !reread.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("updated_at went backwards: %v then %v", created.UpdatedAt, reread.UpdatedAt)
	}
	if !reread.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at = %v, want the original %v", reread.CreatedAt, created.CreatedAt)
	}
}

// TestPgUpdateStatusPagePasswordBranches walks all three password paths. The
// "set a new hash" branch appends a twelfth argument and renumbers
// password_hash to $12 — the exact place an argument-count mistake hides,
// because the other eleven columns still look plausible.
func TestPgUpdateStatusPagePasswordBranches(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	created, err := s.CreateStatusPage(ctx, fullPage())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	base := StatusPage{
		Slug: "pw-slug", Title: "pw-title", Description: "pw-description",
		FooterText: "pw-footer", Theme: "auto", CustomDomain: "pw.example.test",
		Locale: "en-GB", AccentColor: "#0f0f0f", Published: true, AllowIndexing: true,
	}

	t.Run("preserve", func(t *testing.T) {
		in := base
		in.PasswordHash = ""
		got, err := s.UpdateStatusPage(ctx, created.ID, in)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want := in
		want.PasswordHash = created.PasswordHash
		want.LogoPath = testLogoPath
		assertPageColumns(t, got, want)
	})

	t.Run("replace", func(t *testing.T) {
		in := base
		in.PasswordHash = "argon2id$replaced"
		// Change every other column too, so a renumbering that shifts args
		// cannot be masked by unchanged neighbours.
		in.Slug, in.Title = "pw2-slug", "pw2-title"
		in.Description, in.FooterText = "pw2-description", "pw2-footer"
		in.Theme, in.Locale = "dark", "es-ES"
		in.CustomDomain, in.AccentColor = "pw2.example.test", "#abcdef"
		in.Published, in.AllowIndexing = false, false

		got, err := s.UpdateStatusPage(ctx, created.ID, in)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want := in
		want.LogoPath = testLogoPath
		assertPageColumns(t, got, want)

		reread, err := s.GetStatusPage(ctx, created.ID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		assertPageColumns(t, reread, want)
	})

	t.Run("clear", func(t *testing.T) {
		in := base
		in.PasswordHash = passwordClearSentinel
		got, err := s.UpdateStatusPage(ctx, created.ID, in)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want := in
		want.PasswordHash = ""
		want.LogoPath = testLogoPath
		assertPageColumns(t, got, want)
		if got.HasPassword {
			t.Errorf("has_password = true after clearing")
		}
	})
}

// TestPgCreateStatusPageClearSentinel documents that the sentinel is treated as
// "no password" on create rather than being stored literally.
func TestPgCreateStatusPageClearSentinel(t *testing.T) {
	s := newPgStore(t)
	in := fullPage()
	in.PasswordHash = passwordClearSentinel
	got, err := s.CreateStatusPage(context.Background(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.PasswordHash != "" || got.HasPassword {
		t.Fatalf("password_hash = %q, has_password = %v, want empty and false", got.PasswordHash, got.HasPassword)
	}
}

func TestPgIncrementStatusPageViews(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	created, err := s.CreateStatusPage(ctx, fullPage())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := range 3 {
		if err := s.IncrementStatusPageViews(ctx, created.ID); err != nil {
			t.Fatalf("increment %d: %v", i, err)
		}
	}
	got, err := s.GetStatusPage(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ViewCount != 3 {
		t.Fatalf("view_count = %d, want 3", got.ViewCount)
	}
	if err := s.IncrementStatusPageViews(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("increment unknown page = %v, want ErrNotFound", err)
	}
}

// --- logo ---

func TestPgSetStatusPageLogoSetReplaceClear(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	in := fullPage()
	in.LogoPath = "" // start without one
	created, err := s.CreateStatusPage(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.LogoPath != "" {
		t.Fatalf("logo_path = %q on a page created without one", created.LogoPath)
	}

	// Set.
	page, prev, err := s.SetStatusPageLogo(ctx, created.ID, testLogoPath)
	if err != nil {
		t.Fatalf("set logo: %v", err)
	}
	if prev != "" {
		t.Errorf("replaced path = %q, want empty on the first upload", prev)
	}
	if page.LogoPath != testLogoPath {
		t.Errorf("logo_path = %q, want %q", page.LogoPath, testLogoPath)
	}

	// Replace: the returned previous path is what cleanup deletes from disk, so
	// it must be the value this write actually displaced.
	page, prev, err = s.SetStatusPageLogo(ctx, created.ID, testLogoPath2)
	if err != nil {
		t.Fatalf("replace logo: %v", err)
	}
	if prev != testLogoPath {
		t.Errorf("replaced path = %q, want %q", prev, testLogoPath)
	}
	if page.LogoPath != testLogoPath2 {
		t.Errorf("logo_path = %q, want %q", page.LogoPath, testLogoPath2)
	}

	// Clear.
	page, prev, err = s.SetStatusPageLogo(ctx, created.ID, "")
	if err != nil {
		t.Fatalf("clear logo: %v", err)
	}
	if prev != testLogoPath2 {
		t.Errorf("replaced path = %q, want %q", prev, testLogoPath2)
	}
	if page.LogoPath != "" {
		t.Errorf("logo_path = %q, want empty after a clear", page.LogoPath)
	}
	// The column must be NULL, not the empty string: the readers distinguish
	// them only by accident today, and a '' would defeat nilIfEmpty.
	var stored *string
	if err := testPool.QueryRow(ctx, `SELECT logo_path FROM status_pages WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("read logo_path: %v", err)
	}
	if stored != nil {
		t.Errorf("logo_path column = %q, want NULL", *stored)
	}

	if _, _, err := s.SetStatusPageLogo(ctx, 999999, testLogoPath); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set logo on unknown page = %v, want ErrNotFound", err)
	}
}

// TestPgSetStatusPageLogoLeavesOtherColumns proves the narrow write really is
// narrow — it shares the pageCols RETURNING clause with the full update, so a
// stray column in its SET clause would be invisible to the type system.
func TestPgSetStatusPageLogoLeavesOtherColumns(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	in := fullPage()
	in.LogoPath = ""
	created, err := s.CreateStatusPage(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := insertMonitor(t, "logo-monitor")
	if _, err := s.UpdateStatusPage(ctx, created.ID, StatusPage{
		Slug: in.Slug, Title: in.Title, Description: in.Description, FooterText: in.FooterText,
		Theme: in.Theme, CustomDomain: in.CustomDomain, Locale: in.Locale,
		AccentColor: in.AccentColor, Published: in.Published, AllowIndexing: in.AllowIndexing,
		Groups:   []StatusPageGroup{{Name: "G", SortOrder: 0}},
		Monitors: []StatusPageMonitorEntry{{MonitorID: m, GroupName: "G", ShowUptime: true}},
	}); err != nil {
		t.Fatalf("seed layout: %v", err)
	}

	before, err := s.GetStatusPage(ctx, created.ID)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	after, _, err := s.SetStatusPageLogo(ctx, created.ID, testLogoPath)
	if err != nil {
		t.Fatalf("set logo: %v", err)
	}
	want := before
	want.LogoPath = testLogoPath
	assertPageColumns(t, after, want)
	if !reflect.DeepEqual(after.Groups, before.Groups) {
		t.Errorf("groups changed by a logo write: %+v, want %+v", after.Groups, before.Groups)
	}
	if !reflect.DeepEqual(after.Monitors, before.Monitors) {
		t.Errorf("monitors changed by a logo write: %+v, want %+v", after.Monitors, before.Monitors)
	}
	if before.ViewCount != after.ViewCount {
		t.Errorf("view_count changed by a logo write: %d, want %d", after.ViewCount, before.ViewCount)
	}
}

// --- constraints ---

func TestPgStatusPageUniqueViolationsMapToErrConflict(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	first := fullPage()
	if _, err := s.CreateStatusPage(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("duplicate slug", func(t *testing.T) {
		dup := fullPage()
		dup.CustomDomain = "other.example.test"
		if _, err := s.CreateStatusPage(ctx, dup); !errors.Is(err, ErrConflict) {
			t.Fatalf("create with duplicate slug = %v, want ErrConflict", err)
		}
	})

	t.Run("duplicate custom domain, different case", func(t *testing.T) {
		// status_pages_custom_domain_idx is UNIQUE on lower(custom_domain), so a
		// differently-cased duplicate must still conflict.
		dup := fullPage()
		dup.Slug = "other-slug"
		dup.CustomDomain = "ROUNDTRIP.EXAMPLE.TEST"
		if _, err := s.CreateStatusPage(ctx, dup); !errors.Is(err, ErrConflict) {
			t.Fatalf("create with case-varied duplicate domain = %v, want ErrConflict", err)
		}
	})

	t.Run("update onto an existing slug", func(t *testing.T) {
		other := fullPage()
		other.Slug = "second-slug"
		other.CustomDomain = "second.example.test"
		created, err := s.CreateStatusPage(ctx, other)
		if err != nil {
			t.Fatalf("create second: %v", err)
		}
		clash := other
		clash.Slug = first.Slug
		if _, err := s.UpdateStatusPage(ctx, created.ID, clash); !errors.Is(err, ErrConflict) {
			t.Fatalf("update onto an existing slug = %v, want ErrConflict", err)
		}
	})

	t.Run("two pages may both have no domain", func(t *testing.T) {
		// The unique index is partial (WHERE custom_domain IS NOT NULL); nilIfEmpty
		// is what keeps a second domain-less page from colliding on ''.
		for i, slug := range []string{"nodomain-a", "nodomain-b"} {
			p := fullPage()
			p.Slug, p.CustomDomain = slug, ""
			if _, err := s.CreateStatusPage(ctx, p); err != nil {
				t.Fatalf("create domain-less page %d: %v", i, err)
			}
		}
	})
}

// TestPgStatusPageCheckViolations pins the current mapping: CHECK failures are
// NOT ErrConflict and NOT ErrNotFound. They surface as the raw SQLSTATE, which
// handlers turn into a 500 — the schema is the last line of defence behind the
// Go-side normalisation, so reaching it is a bug, not a user error.
func TestPgStatusPageCheckViolations(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()

	t.Run("accent_color", func(t *testing.T) {
		p := fullPage()
		p.Slug, p.CustomDomain = "bad-accent", "bad-accent.example.test"
		p.AccentColor = "not-a-colour"
		_, err := s.CreateStatusPage(ctx, p)
		if err == nil {
			t.Fatalf("create with an invalid accent colour succeeded")
		}
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			t.Fatalf("check violation mapped to a store sentinel: %v", err)
		}
		code, constraint := pgErrorCode(t, err)
		if code != "23514" {
			t.Errorf("sqlstate = %s, want 23514 (check_violation)", code)
		}
		if constraint != "status_pages_accent_color_chk" {
			t.Errorf("constraint = %q, want status_pages_accent_color_chk", constraint)
		}
	})

	t.Run("accent_color uppercase is rejected", func(t *testing.T) {
		// The CHECK is lowercase-only; normalizeHexColor is what guarantees Go
		// never sends "#AABBCC".
		p := fullPage()
		p.Slug, p.CustomDomain = "upper-accent", "upper-accent.example.test"
		p.AccentColor = "#AABBCC"
		if _, err := s.CreateStatusPage(ctx, p); err == nil {
			t.Fatalf("create with an uppercase accent colour succeeded")
		}
	})

	t.Run("theme", func(t *testing.T) {
		p := fullPage()
		p.Slug, p.CustomDomain = "bad-theme", "bad-theme.example.test"
		p.Theme = "neon"
		_, err := s.CreateStatusPage(ctx, p)
		if err == nil {
			t.Fatalf("create with an invalid theme succeeded")
		}
		if code, _ := pgErrorCode(t, err); code != "23514" {
			t.Errorf("sqlstate = %s, want 23514 (check_violation)", code)
		}
	})
}

func TestPgStatusPageNotFound(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	if _, err := s.GetStatusPage(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown id = %v, want ErrNotFound", err)
	}
	if _, err := s.GetStatusPageBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown slug = %v, want ErrNotFound", err)
	}
	if _, err := s.GetStatusPageByDomain(ctx, "nope.example.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown domain = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateStatusPage(ctx, 999999, fullPage()); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown id = %v, want ErrNotFound", err)
	}
	if err := s.DeleteStatusPage(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown id = %v, want ErrNotFound", err)
	}
	if _, err := s.ReorderStatusPageLayout(ctx, 999999, StatusPageLayoutOrder{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("reorder unknown id = %v, want ErrNotFound", err)
	}
}

// TestPgDeleteStatusPageCascades proves the ON DELETE CASCADE in 0001 really
// removes the layout rows; an orphaned status_page_monitors row would break the
// next page that reused the id.
func TestPgDeleteStatusPageCascades(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	if err := s.DeleteStatusPage(ctx, f.page.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, table := range []string{"status_page_monitors", "status_page_groups"} {
		var n int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE status_page_id = $1`, f.page.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows for the deleted page", table, n)
		}
	}
}

// --- layout ---

// pgLayoutFixture is the live-database twin of layoutFixture: group "A" holds
// m1 then m2, group "B" holds m3, nothing ungrouped.
type pgLayoutFixture struct {
	store      *PgStore
	page       StatusPage
	m1, m2, m3 int64
	gA, gB     int64
}

func newPgLayoutFixture(t *testing.T, s *PgStore) *pgLayoutFixture {
	t.Helper()
	m1 := insertMonitor(t, "one")
	m2 := insertMonitor(t, "two")
	m3 := insertMonitor(t, "three")
	page, err := s.CreateStatusPage(context.Background(), StatusPage{
		Slug: "layout", Title: "Layout", Theme: "auto", Published: true,
		Groups: []StatusPageGroup{{Name: "A", SortOrder: 0}, {Name: "B", SortOrder: 1}},
		Monitors: []StatusPageMonitorEntry{
			{MonitorID: m1, GroupName: "A", SortOrder: 0, ShowUptime: true},
			{MonitorID: m2, GroupName: "A", SortOrder: 1, ShowUptime: true},
			{MonitorID: m3, GroupName: "B", SortOrder: 2, ShowUptime: true},
		},
	})
	if err != nil {
		t.Fatalf("create layout page: %v", err)
	}
	f := &pgLayoutFixture{store: s, page: page, m1: m1, m2: m2, m3: m3}
	f.gA = pgGroupID(t, page, "A")
	f.gB = pgGroupID(t, page, "B")
	return f
}

func pgGroupID(t *testing.T, p StatusPage, name string) int64 {
	t.Helper()
	for _, g := range p.Groups {
		if g.Name == name {
			return g.ID
		}
	}
	t.Fatalf("group %q not on page %d: %+v", name, p.ID, p.Groups)
	return 0
}

func pgEntryOrder(p StatusPage) []int64 {
	ids := make([]int64, 0, len(p.Monitors))
	for _, e := range p.Monitors {
		ids = append(ids, e.MonitorID)
	}
	return ids
}

func TestPgReorderStatusPageLayout(t *testing.T) {
	s := newPgStore(t)
	f := newPgLayoutFixture(t, s)

	got, err := s.ReorderStatusPageLayout(context.Background(), f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
			{GroupID: f.gA, MonitorIDs: []int64{f.m2, f.m1}},
		},
	})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if len(got.Groups) != 2 || got.Groups[0].Name != "B" || got.Groups[1].Name != "A" {
		t.Fatalf("group order = %+v, want B then A", got.Groups)
	}
	want := []int64{f.m3, f.m2, f.m1}
	if !reflect.DeepEqual(pgEntryOrder(got), want) {
		t.Fatalf("monitor order = %v, want %v", pgEntryOrder(got), want)
	}
	// One contiguous running position across the whole layout — the property
	// loadPageLayout's `ORDER BY sort_order, monitor_id` depends on.
	for i, e := range got.Monitors {
		if e.SortOrder != i {
			t.Errorf("monitors[%d].sort_order = %d, want %d", i, e.SortOrder, i)
		}
	}
	if !reflect.DeepEqual(got.MonitorIDs, want) {
		t.Errorf("monitor_ids = %v, want %v", got.MonitorIDs, want)
	}
}

func TestPgReorderStatusPageLayoutRejectsBadPayloads(t *testing.T) {
	ctx := context.Background()

	// Each case gets its own truncated schema and fixture, so a case that did
	// mutate the layout cannot leak into the next one's "unchanged" assertion.
	cases := map[string]func(f *pgLayoutFixture) StatusPageLayoutOrder{
		"incomplete monitor set": func(f *pgLayoutFixture) StatusPageLayoutOrder {
			return StatusPageLayoutOrder{Groups: []StatusPageGroupOrder{
				{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
				{GroupID: f.gB},
			}}
		},
		"incomplete group set": func(f *pgLayoutFixture) StatusPageLayoutOrder {
			return StatusPageLayoutOrder{Groups: []StatusPageGroupOrder{
				{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2, f.m3}},
			}}
		},
		"duplicate group id": func(f *pgLayoutFixture) StatusPageLayoutOrder {
			return StatusPageLayoutOrder{Groups: []StatusPageGroupOrder{
				{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
				{GroupID: f.gA, MonitorIDs: []int64{f.m3}},
			}}
		},
		"duplicate monitor id": func(f *pgLayoutFixture) StatusPageLayoutOrder {
			return StatusPageLayoutOrder{
				Ungrouped: []int64{f.m1},
				Groups: []StatusPageGroupOrder{
					{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
					{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
				},
			}
		},
		"unknown group id": func(f *pgLayoutFixture) StatusPageLayoutOrder {
			return StatusPageLayoutOrder{Groups: []StatusPageGroupOrder{
				{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
				{GroupID: 999999, MonitorIDs: []int64{f.m3}},
			}}
		},
		"unknown monitor id": func(f *pgLayoutFixture) StatusPageLayoutOrder {
			return StatusPageLayoutOrder{Groups: []StatusPageGroupOrder{
				{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
				{GroupID: f.gB, MonitorIDs: []int64{999999}},
			}}
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s := newPgStore(t)
			f := newPgLayoutFixture(t, s)
			before, err := s.GetStatusPage(ctx, f.page.ID)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			if _, err := s.ReorderStatusPageLayout(ctx, f.page.ID, build(f)); !errors.Is(err, ErrConflict) {
				t.Fatalf("reorder = %v, want ErrConflict", err)
			}
			// The whole reorder runs in one transaction, so a rejection must
			// leave nothing behind — not even the updates that ran before the
			// offending row was reached.
			after, err := s.GetStatusPage(ctx, f.page.ID)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if !reflect.DeepEqual(after.Monitors, before.Monitors) {
				t.Errorf("monitors changed after a rejected reorder: %+v, want %+v", after.Monitors, before.Monitors)
			}
			if !reflect.DeepEqual(after.Groups, before.Groups) {
				t.Errorf("groups changed after a rejected reorder: %+v, want %+v", after.Groups, before.Groups)
			}
		})
	}
}

// TestPgReorderStatusPageLayoutRejectsForeignIDs uses ids that exist but belong
// to another page. The `AND status_page_id = $2` guard on the group UPDATE is
// what makes this ErrConflict instead of a cross-page write.
func TestPgReorderStatusPageLayoutRejectsForeignIDs(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	other := insertMonitor(t, "other")
	page2, err := s.CreateStatusPage(ctx, StatusPage{
		Slug: "other", Title: "Other", Theme: "auto", Published: true,
		Groups:   []StatusPageGroup{{Name: "C", SortOrder: 0}},
		Monitors: []StatusPageMonitorEntry{{MonitorID: other, GroupName: "C", ShowUptime: true}},
	})
	if err != nil {
		t.Fatalf("create page 2: %v", err)
	}
	page2Group := pgGroupID(t, page2, "C")
	page2Before, err := s.GetStatusPage(ctx, page2.ID)
	if err != nil {
		t.Fatalf("read page 2: %v", err)
	}

	// Page 2's group id in page 1's payload, count still matching.
	if _, err := s.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: page2Group, MonitorIDs: []int64{f.m3}},
		},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign group id = %v, want ErrConflict", err)
	}

	// Page 2's monitor id in place of one of page 1's, count still matching.
	if _, err := s.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: f.gB, MonitorIDs: []int64{other}},
		},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign monitor id = %v, want ErrConflict", err)
	}

	page2After, err := s.GetStatusPage(ctx, page2.ID)
	if err != nil {
		t.Fatalf("re-read page 2: %v", err)
	}
	if !reflect.DeepEqual(page2After.Groups, page2Before.Groups) ||
		!reflect.DeepEqual(page2After.Monitors, page2Before.Monitors) {
		t.Fatalf("page 2 layout was written across pages: %+v", page2After)
	}
}

// TestPgReorderStatusPageLayoutBlocksOnRowLock is the direct test of the
// `SELECT id ... FOR UPDATE` in ReorderStatusPageLayout. With the page row held
// by another transaction the reorder must wait rather than proceed on a stale
// read; dropping FOR UPDATE turns this into an immediate success.
func TestPgReorderStatusPageLayoutBlocksOnRowLock(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	var locked int64
	if err := holder.QueryRow(ctx, `SELECT id FROM status_pages WHERE id = $1 FOR UPDATE`, f.page.ID).Scan(&locked); err != nil {
		t.Fatalf("hold row lock: %v", err)
	}

	order := StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
			{GroupID: f.gA, MonitorIDs: []int64{f.m2, f.m1}},
		},
	}

	// While the lock is held the reorder must not complete.
	blockedCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if _, err := s.ReorderStatusPageLayout(blockedCtx, f.page.ID, order); err == nil {
		_ = holder.Rollback(ctx)
		t.Fatalf("reorder completed while another transaction held the page row; FOR UPDATE is not taking effect")
	}

	// Once the holder releases, the same reorder succeeds.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("rollback holder: %v", err)
	}
	got, err := s.ReorderStatusPageLayout(ctx, f.page.ID, order)
	if err != nil {
		t.Fatalf("reorder after the lock was released: %v", err)
	}
	want := []int64{f.m3, f.m2, f.m1}
	if !reflect.DeepEqual(pgEntryOrder(got), want) {
		t.Fatalf("monitor order = %v, want %v", pgEntryOrder(got), want)
	}
}

// TestPgReorderStatusPageLayoutConcurrent runs competing reorders against one
// page. The row lock serialises them, so whichever wins, the committed layout
// must be exactly one of the submitted orderings with a contiguous sort_order —
// never a half-applied interleaving of both.
func TestPgReorderStatusPageLayoutConcurrent(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	orders := []StatusPageLayoutOrder{
		{Groups: []StatusPageGroupOrder{
			{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
		}},
		{Groups: []StatusPageGroupOrder{
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
			{GroupID: f.gA, MonitorIDs: []int64{f.m2, f.m1}},
		}},
		{Groups: []StatusPageGroupOrder{
			{GroupID: f.gA, MonitorIDs: []int64{f.m2, f.m1}},
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
		}},
	}
	// Every ordering is individually valid, so every call must succeed: a
	// deadlock or a lost update would show up as an error here.
	const rounds = 4
	var wg sync.WaitGroup
	errCh := make(chan error, len(orders)*rounds)
	for range rounds {
		for _, o := range orders {
			wg.Add(1)
			go func(o StatusPageLayoutOrder) {
				defer wg.Done()
				if _, err := s.ReorderStatusPageLayout(ctx, f.page.ID, o); err != nil {
					errCh <- err
				}
			}(o)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent reorder: %v", err)
	}

	final, err := s.GetStatusPage(ctx, f.page.ID)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	for i, e := range final.Monitors {
		if e.SortOrder != i {
			t.Fatalf("monitors[%d].sort_order = %d, want %d — a concurrent reorder committed a partial layout: %+v",
				i, e.SortOrder, i, final.Monitors)
		}
	}
	got := pgEntryOrder(final)
	valid := [][]int64{
		{f.m1, f.m2, f.m3},
		{f.m3, f.m2, f.m1},
		{f.m2, f.m1, f.m3},
	}
	for _, w := range valid {
		if reflect.DeepEqual(got, w) {
			return
		}
	}
	t.Fatalf("final order %v is none of the submitted orderings %v", got, valid)
}

// TestPgUpdateStatusPageRecreatesLayout pins the full-replace semantics the
// reorder's optimistic-concurrency story depends on: a save issues new group
// ids, which is what makes a reorder computed against the old ones conflict.
func TestPgUpdateStatusPageRecreatesLayout(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	saved, err := s.UpdateStatusPage(ctx, f.page.ID, StatusPage{
		Slug: "layout", Title: "Layout", Theme: "auto", Published: true,
		Groups: []StatusPageGroup{{Name: "A", SortOrder: 0}, {Name: "B", SortOrder: 1}},
		Monitors: []StatusPageMonitorEntry{
			{MonitorID: f.m1, GroupName: "A", SortOrder: 0, ShowUptime: true},
			{MonitorID: f.m2, GroupName: "A", SortOrder: 1, ShowUptime: true},
			{MonitorID: f.m3, GroupName: "B", SortOrder: 2, ShowUptime: true},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if pgGroupID(t, saved, "A") == f.gA {
		t.Fatalf("group ids survived a save; the optimistic-concurrency property is gone")
	}
	if _, err := s.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
			{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
		},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reorder with stale group ids = %v, want ErrConflict", err)
	}
}

// TestPgReorderStatusPageLayoutPreservesShowUptime proves a reorder rewrites
// placement only. show_uptime is in the same UPDATE's neighbourhood, so a
// careless SET clause would reset it.
func TestPgReorderStatusPageLayoutPreservesShowUptime(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	m1 := insertMonitor(t, "one")
	m2 := insertMonitor(t, "two")
	page, err := s.CreateStatusPage(ctx, StatusPage{
		Slug: "cross", Title: "Cross", Theme: "auto", Published: true,
		Groups: []StatusPageGroup{{Name: "A", SortOrder: 0}, {Name: "B", SortOrder: 1}},
		Monitors: []StatusPageMonitorEntry{
			{MonitorID: m1, GroupName: "A", SortOrder: 0, ShowUptime: false},
			{MonitorID: m2, GroupName: "B", SortOrder: 1, ShowUptime: true},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	gA, gB := pgGroupID(t, page, "A"), pgGroupID(t, page, "B")

	got, err := s.ReorderStatusPageLayout(ctx, page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: gA},
			{GroupID: gB, MonitorIDs: []int64{m2, m1}},
		},
	})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	var moved *StatusPageMonitorEntry
	for i := range got.Monitors {
		if got.Monitors[i].MonitorID == m1 {
			moved = &got.Monitors[i]
		}
	}
	if moved == nil {
		t.Fatalf("m1 missing from the layout: %+v", got.Monitors)
	}
	if moved.GroupID == nil || *moved.GroupID != gB || moved.GroupName != "B" {
		t.Fatalf("moved entry = %+v, want group B (%d)", moved, gB)
	}
	if moved.ShowUptime {
		t.Errorf("show_uptime was reset by the reorder")
	}
}

// TestPgReorderStatusPageLayoutUngrouped covers the nil-group_id path: the
// UPDATE binds a NULL for group_id, which pgx must send as an untyped NULL
// against a bigint column.
func TestPgReorderStatusPageLayoutUngrouped(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	got, err := s.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Ungrouped: []int64{f.m3, f.m1, f.m2},
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gA},
			{GroupID: f.gB},
		},
	})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	want := []int64{f.m3, f.m1, f.m2}
	if !reflect.DeepEqual(pgEntryOrder(got), want) {
		t.Fatalf("monitor order = %v, want %v", pgEntryOrder(got), want)
	}
	for _, e := range got.Monitors {
		if e.GroupID != nil || e.GroupName != "" {
			t.Errorf("entry %d still grouped: %+v", e.MonitorID, e)
		}
	}
}

func TestPgReorderStatusPageLayoutEmptyPage(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	page, err := s.CreateStatusPage(ctx, StatusPage{
		Slug: "bare", Title: "Bare", Theme: "auto", Published: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ReorderStatusPageLayout(ctx, page.ID, StatusPageLayoutOrder{}); err != nil {
		t.Fatalf("empty reorder: %v", err)
	}
}

// TestPgReorderStatusPageLayoutBumpsUpdatedAt covers the trailing
// `UPDATE status_pages SET updated_at = now()`; without it a reorder would be
// invisible to any ETag or cache keyed on the page's timestamp.
func TestPgReorderStatusPageLayoutBumpsUpdatedAt(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	f := newPgLayoutFixture(t, s)

	// now() is the transaction start time, so two statements in the same
	// millisecond still differ only if the transactions differ — they do here,
	// but pin the comparison to "not before" plus an explicit change check.
	before, err := s.GetStatusPage(ctx, f.page.ID)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	got, err := s.ReorderStatusPageLayout(ctx, f.page.ID, StatusPageLayoutOrder{
		Groups: []StatusPageGroupOrder{
			{GroupID: f.gB, MonitorIDs: []int64{f.m3}},
			{GroupID: f.gA, MonitorIDs: []int64{f.m1, f.m2}},
		},
	})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if !got.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("updated_at = %s, want after %s", got.UpdatedAt, before.UpdatedAt)
	}
}
