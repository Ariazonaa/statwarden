package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- maintenance windows ---

const maintCols = `id, title, description, active, strategy, starts_at, ends_at, recurrence, timezone, applies_to_all, created_at, updated_at`

func scanMaintenance(row pgx.Row) (MaintenanceWindow, error) {
	var m MaintenanceWindow
	var starts, ends *time.Time
	var recurrence []byte
	err := row.Scan(&m.ID, &m.Title, &m.Description, &m.Active, &m.Strategy, &starts, &ends, &recurrence, &m.Timezone, &m.AppliesToAll, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return MaintenanceWindow{}, err
	}
	m.StartsAt, m.EndsAt = starts, ends
	if len(recurrence) > 0 {
		m.Recurrence = json.RawMessage(recurrence)
	}
	return m, nil
}

func (s *PgStore) CreateMaintenance(ctx context.Context, m MaintenanceWindow) (MaintenanceWindow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	out, err := scanMaintenance(tx.QueryRow(ctx, `
		INSERT INTO maintenance_windows (title, description, active, strategy, starts_at, ends_at, recurrence, timezone, applies_to_all)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+maintCols,
		m.Title, m.Description, m.Active, m.Strategy, m.StartsAt, m.EndsAt, jsonOrNil(m.Recurrence), m.Timezone, m.AppliesToAll))
	if err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	if err := replaceMaintenanceMonitors(ctx, tx, out.ID, m.MonitorIDs); err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	out.MonitorIDs = idList(m.MonitorIDs)
	return out, mapErr(tx.Commit(ctx))
}

func (s *PgStore) ListMaintenance(ctx context.Context) ([]MaintenanceWindow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+maintCols+` FROM maintenance_windows ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []MaintenanceWindow{}
	for rows.Next() {
		m, err := scanMaintenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach the monitor scoping. GetMaintenance always did this but the list
	// did not, so every window read through here looked unscoped — i.e.
	// applying to ALL monitors. That is the difference between suppressing one
	// monitor's alerts and suppressing everybody's, so it is loaded here rather
	// than left to callers. One extra query for the whole list, not one per row.
	scopes, err := s.maintenanceMonitorScopes(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	for i := range out {
		// idList, not a bare map read: a window with no links is simply absent
		// from scopes, so the lookup yields nil — and an unscoped window is the
		// fleet-wide case, i.e. the one this list shows most often.
		out[i].MonitorIDs = idList(scopes[out[i].ID])
	}
	return out, nil
}

// maintenanceMonitorScopes reads every maintenance→monitor link in one query,
// keyed by maintenance id.
func (s *PgStore) maintenanceMonitorScopes(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT maintenance_id, monitor_id FROM maintenance_monitors ORDER BY maintenance_id, monitor_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var id, mid int64
		if err := rows.Scan(&id, &mid); err != nil {
			return nil, err
		}
		out[id] = append(out[id], mid)
	}
	return out, rows.Err()
}

func (s *PgStore) GetMaintenance(ctx context.Context, id int64) (MaintenanceWindow, error) {
	m, err := scanMaintenance(s.pool.QueryRow(ctx, `SELECT `+maintCols+` FROM maintenance_windows WHERE id = $1`, id))
	if err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	ids, err := s.maintenanceMonitorIDs(ctx, id)
	if err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	m.MonitorIDs = ids
	return m, nil
}

func (s *PgStore) maintenanceMonitorIDs(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT monitor_id FROM maintenance_monitors WHERE maintenance_id = $1 ORDER BY monitor_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Empty, never nil: a window with no rows here is one that applies to every
	// monitor, which is the ordinary way to schedule fleet-wide downtime, and it
	// is serialised straight into monitor_ids.
	ids := []int64{}
	for rows.Next() {
		var mid int64
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		ids = append(ids, mid)
	}
	return ids, rows.Err()
}

func (s *PgStore) UpdateMaintenance(ctx context.Context, id int64, m MaintenanceWindow) (MaintenanceWindow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	out, err := scanMaintenance(tx.QueryRow(ctx, `
		UPDATE maintenance_windows SET title=$2, description=$3, active=$4, strategy=$5, starts_at=$6,
			ends_at=$7, recurrence=$8, timezone=$9, applies_to_all=$10, updated_at=now()
		WHERE id=$1 RETURNING `+maintCols,
		id, m.Title, m.Description, m.Active, m.Strategy, m.StartsAt, m.EndsAt, jsonOrNil(m.Recurrence), m.Timezone, m.AppliesToAll))
	if err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	if err := replaceMaintenanceMonitors(ctx, tx, id, m.MonitorIDs); err != nil {
		return MaintenanceWindow{}, mapErr(err)
	}
	out.MonitorIDs = idList(m.MonitorIDs)
	return out, mapErr(tx.Commit(ctx))
}

func (s *PgStore) DeleteMaintenance(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM maintenance_windows WHERE id = $1`, id)
	return affected(ct, err)
}

func replaceMaintenanceMonitors(ctx context.Context, tx pgx.Tx, id int64, monitorIDs []int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM maintenance_monitors WHERE maintenance_id = $1`, id); err != nil {
		return err
	}
	for _, mid := range monitorIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO maintenance_monitors (maintenance_id, monitor_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, mid); err != nil {
			return err
		}
	}
	return nil
}

// --- status pages ---

const pageCols = `id, slug, title, description, footer_text, theme, logo_path, custom_domain,
	locale, published, password_hash, allow_indexing, view_count, created_at, updated_at, accent_color`

func scanStatusPage(row pgx.Row) (StatusPage, error) {
	var p StatusPage
	var logo, domain, locale, passwordHash, accent *string
	err := row.Scan(&p.ID, &p.Slug, &p.Title, &p.Description, &p.FooterText, &p.Theme,
		&logo, &domain, &locale, &p.Published, &passwordHash, &p.AllowIndexing, &p.ViewCount,
		&p.CreatedAt, &p.UpdatedAt, &accent)
	if err != nil {
		return StatusPage{}, err
	}
	// Sanitised at the store boundary so no reader — public view, admin list,
	// admin detail — can serialise a legacy free-text logo_path into an
	// <img src>. Doing it here rather than per-handler is what makes that
	// hold for handlers written later.
	p.LogoPath = sanitizeLogoPath(strDeref(logo))
	p.CustomDomain = strDeref(domain)
	p.Locale = strDeref(locale)
	p.PasswordHash = strDeref(passwordHash)
	p.HasPassword = p.PasswordHash != ""
	p.AccentColor = strDeref(accent)
	return p, nil
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) CreateStatusPage(ctx context.Context, p StatusPage) (StatusPage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if p.PasswordHash == passwordClearSentinel {
		p.PasswordHash = ""
	}
	out, err := scanStatusPage(tx.QueryRow(ctx, `
		INSERT INTO status_pages (slug, title, description, footer_text, theme, logo_path, custom_domain, locale, published, password_hash, allow_indexing, accent_color)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+pageCols,
		p.Slug, p.Title, p.Description, p.FooterText, p.Theme, nilIfEmpty(p.LogoPath), nilIfEmpty(p.CustomDomain),
		nilIfEmpty(p.Locale), p.Published, nilIfEmpty(p.PasswordHash), p.AllowIndexing, nilIfEmpty(p.AccentColor)))
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	if err := replacePageLayout(ctx, tx, out.ID, p); err != nil {
		return StatusPage{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return StatusPage{}, err
	}
	return s.GetStatusPage(ctx, out.ID)
}

func (s *PgStore) ListStatusPages(ctx context.Context) ([]StatusPage, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pageCols+` FROM status_pages ORDER BY id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []StatusPage{}
	for rows.Next() {
		p, err := scanStatusPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	for i := range out {
		if err := s.loadPageLayout(ctx, &out[i]); err != nil {
			return nil, mapErr(err)
		}
	}
	return out, nil
}

func (s *PgStore) GetStatusPage(ctx context.Context, id int64) (StatusPage, error) {
	p, err := scanStatusPage(s.pool.QueryRow(ctx, `SELECT `+pageCols+` FROM status_pages WHERE id = $1`, id))
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	return p, mapErr(s.loadPageLayout(ctx, &p))
}

func (s *PgStore) GetStatusPageBySlug(ctx context.Context, slug string) (StatusPage, error) {
	p, err := scanStatusPage(s.pool.QueryRow(ctx, `SELECT `+pageCols+` FROM status_pages WHERE slug = $1`, slug))
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	return p, mapErr(s.loadPageLayout(ctx, &p))
}

func (s *PgStore) GetStatusPageByDomain(ctx context.Context, host string) (StatusPage, error) {
	p, err := scanStatusPage(s.pool.QueryRow(ctx, `SELECT `+pageCols+` FROM status_pages WHERE lower(custom_domain) = lower($1)`, host))
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	return p, mapErr(s.loadPageLayout(ctx, &p))
}

func (s *PgStore) IncrementStatusPageViews(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `UPDATE status_pages SET view_count = view_count + 1 WHERE id = $1`, id)
	return affected(ct, err)
}

// loadPageLayout fills Groups, Monitors and the flat MonitorIDs projection.
func (s *PgStore) loadPageLayout(ctx context.Context, p *StatusPage) error {
	groups, err := s.pageGroups(ctx, p.ID)
	if err != nil {
		return err
	}
	p.Groups = groups
	groupName := map[int64]string{}
	for _, g := range groups {
		groupName[g.ID] = g.Name
	}
	rows, err := s.pool.Query(ctx, `SELECT monitor_id, group_id, sort_order, show_uptime
		FROM status_page_monitors WHERE status_page_id = $1 ORDER BY sort_order, monitor_id`, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var entries []StatusPageMonitorEntry
	for rows.Next() {
		var e StatusPageMonitorEntry
		var gid *int64
		if err := rows.Scan(&e.MonitorID, &gid, &e.SortOrder, &e.ShowUptime); err != nil {
			return err
		}
		if gid != nil {
			e.GroupID = gid
			e.GroupName = groupName[*gid]
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	p.Monitors = entries
	p.MonitorIDs = entryMonitorIDs(entries)
	return nil
}

func (s *PgStore) pageGroups(ctx context.Context, pageID int64) ([]StatusPageGroup, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, status_page_id, name, sort_order FROM status_page_groups WHERE status_page_id = $1 ORDER BY sort_order, id`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatusPageGroup{}
	for rows.Next() {
		var g StatusPageGroup
		if err := rows.Scan(&g.ID, &g.StatusPageID, &g.Name, &g.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PgStore) UpdateStatusPage(ctx context.Context, id int64, p StatusPage) (StatusPage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	// Preserve the existing password when the caller did not supply a new one.
	passwordExpr := `password_hash`
	// logo_path is deliberately absent from the SET clause: it is server-owned
	// and written only by SetStatusPageLogo, so a full-replace update can no
	// longer clear an uploaded logo and orphan its file on disk.
	args := []any{id, p.Slug, p.Title, p.Description, p.FooterText, p.Theme,
		nilIfEmpty(p.CustomDomain), nilIfEmpty(p.Locale), p.Published, p.AllowIndexing,
		nilIfEmpty(p.AccentColor)}
	switch p.PasswordHash {
	case "":
		// leave password_hash unchanged
	case passwordClearSentinel:
		passwordExpr = `NULL`
	default:
		passwordExpr = `$12`
		args = append(args, p.PasswordHash)
	}
	out, err := scanStatusPage(tx.QueryRow(ctx, `
		UPDATE status_pages SET slug=$2, title=$3, description=$4, footer_text=$5, theme=$6,
			custom_domain=$7, locale=$8, published=$9, allow_indexing=$10, accent_color=$11,
			password_hash=`+passwordExpr+`, updated_at=now()
		WHERE id=$1 RETURNING `+pageCols, args...))
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	if err := replacePageLayout(ctx, tx, id, p); err != nil {
		return StatusPage{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return StatusPage{}, err
	}
	_ = out
	return s.GetStatusPage(ctx, id)
}

// SetStatusPageLogo updates only logo_path. It exists so the upload endpoint
// does not have to round-trip a full page through UpdateStatusPage, which would
// recreate every group and placement, and so that logo_path stays server-owned
// rather than client-writable. It also reports the path it replaced.
func (s *PgStore) SetStatusPageLogo(ctx context.Context, id int64, logoPath string) (StatusPage, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusPage{}, "", mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	// The prior path is read under the row lock the update takes anyway, so two
	// overlapping uploads serialise here: each sees the file the other actually
	// left behind rather than a value read before the body was consumed, and
	// neither ends up unreferenced on disk.
	var prev *string
	if err := tx.QueryRow(ctx, `SELECT logo_path FROM status_pages WHERE id=$1 FOR UPDATE`, id).Scan(&prev); err != nil {
		return StatusPage{}, "", mapErr(err)
	}
	p, err := scanStatusPage(tx.QueryRow(ctx,
		`UPDATE status_pages SET logo_path=$2, updated_at=now() WHERE id=$1 RETURNING `+pageCols,
		id, nilIfEmpty(logoPath)))
	if err != nil {
		return StatusPage{}, "", mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return StatusPage{}, "", err
	}
	// The replaced path is returned raw: it names a file on disk, and LogoStore
	// already ignores anything this server did not issue.
	return p, strDeref(prev), mapErr(s.loadPageLayout(ctx, &p))
}

// StatusPageSlugsForMonitor implements Store. Only published pages are
// returned: an unpublished page answers 404 to the public endpoint, so poking
// its topic could only make anonymous subscribers re-fetch a 404.
func (s *PgStore) StatusPageSlugsForMonitor(ctx context.Context, monitorID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT p.slug
		FROM status_pages p
		JOIN status_page_monitors spm ON spm.status_page_id = p.id
		WHERE spm.monitor_id = $1 AND p.published
		ORDER BY p.slug`, monitorID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

func (s *PgStore) DeleteStatusPage(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM status_pages WHERE id = $1`, id)
	return affected(ct, err)
}

// replacePageLayout rewrites a page's groups and monitor placements. Groups are
// created fresh (matched by name from the incoming payload); monitor entries
// reference them by name or by the flat MonitorIDs fallback.
func replacePageLayout(ctx context.Context, tx pgx.Tx, id int64, p StatusPage) error {
	if _, err := tx.Exec(ctx, `DELETE FROM status_page_monitors WHERE status_page_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM status_page_groups WHERE status_page_id = $1`, id); err != nil {
		return err
	}
	byName := map[string]int64{}
	for _, g := range p.Groups {
		var gid int64
		if err := tx.QueryRow(ctx, `INSERT INTO status_page_groups (status_page_id, name, sort_order) VALUES ($1,$2,$3) RETURNING id`,
			id, g.Name, g.SortOrder).Scan(&gid); err != nil {
			return err
		}
		byName[g.Name] = gid
	}
	entries := p.Monitors
	if len(entries) == 0 {
		for i, mid := range p.MonitorIDs {
			entries = append(entries, StatusPageMonitorEntry{MonitorID: mid, SortOrder: i, ShowUptime: true})
		}
	}
	for i, e := range entries {
		var gid *int64
		if e.GroupName != "" {
			if id2, ok := byName[e.GroupName]; ok {
				gid = &id2
			}
		} else if e.GroupID != nil {
			gid = e.GroupID
		}
		order := e.SortOrder
		if order == 0 {
			order = i
		}
		if _, err := tx.Exec(ctx, `INSERT INTO status_page_monitors (status_page_id, monitor_id, group_id, sort_order, show_uptime)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT (status_page_id, monitor_id) DO UPDATE SET group_id=EXCLUDED.group_id, sort_order=EXCLUDED.sort_order, show_uptime=EXCLUDED.show_uptime`,
			id, e.MonitorID, gid, order, e.ShowUptime); err != nil {
			return err
		}
	}
	return nil
}

// ReorderStatusPageLayout rewrites group and monitor positions in one
// transaction. The page row is locked FOR UPDATE so a concurrent
// UpdateStatusPage (which locks the same row before calling replacePageLayout)
// cannot interleave. Every UPDATE is checked for a non-zero row count, every id
// is checked for distinctness, and the totals must match, so a payload computed
// against a stale layout fails as ErrConflict instead of applying partially.
func (s *PgStore) ReorderStatusPageLayout(ctx context.Context, pageID int64, order StatusPageLayoutOrder) (StatusPage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusPage{}, mapErr(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM status_pages WHERE id = $1 FOR UPDATE`, pageID).Scan(&locked); err != nil {
		return StatusPage{}, mapErr(err)
	}
	// Distinctness plus count: a payload naming the same group twice would pass
	// every RowsAffected check and the count check alone, silently leaving
	// another group with a stale sort_order.
	seenGroups := make(map[int64]struct{}, len(order.Groups))
	for i, g := range order.Groups {
		if _, dup := seenGroups[g.GroupID]; dup {
			return StatusPage{}, ErrConflict
		}
		seenGroups[g.GroupID] = struct{}{}
		ct, err := tx.Exec(ctx, `UPDATE status_page_groups SET sort_order = $3 WHERE id = $1 AND status_page_id = $2`, g.GroupID, pageID, i)
		if err != nil {
			return StatusPage{}, mapErr(err)
		}
		if ct.RowsAffected() == 0 {
			return StatusPage{}, ErrConflict // unknown group, or a group belonging to another page
		}
	}
	var groupCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM status_page_groups WHERE status_page_id = $1`, pageID).Scan(&groupCount); err != nil {
		return StatusPage{}, mapErr(err)
	}
	if groupCount != len(seenGroups) {
		return StatusPage{}, ErrConflict
	}
	// One running position across the whole layout: loadPageLayout reads
	// `ORDER BY sort_order, monitor_id`, so a single contiguous sequence yields
	// both the group order and the order inside each group.
	pos := 0
	placed := map[int64]struct{}{}
	assign := func(gid *int64, ids []int64) error {
		for _, mid := range ids {
			if _, dup := placed[mid]; dup {
				return ErrConflict
			}
			placed[mid] = struct{}{}
			ct, err := tx.Exec(ctx, `UPDATE status_page_monitors SET group_id = $3, sort_order = $4 WHERE status_page_id = $1 AND monitor_id = $2`, pageID, mid, gid, pos)
			if err != nil {
				return mapErr(err)
			}
			if ct.RowsAffected() == 0 {
				return ErrConflict // monitor is not placed on this page
			}
			pos++
		}
		return nil
	}
	if err := assign(nil, order.Ungrouped); err != nil {
		return StatusPage{}, err
	}
	for _, g := range order.Groups {
		gid := g.GroupID
		if err := assign(&gid, g.MonitorIDs); err != nil {
			return StatusPage{}, err
		}
	}
	var monCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM status_page_monitors WHERE status_page_id = $1`, pageID).Scan(&monCount); err != nil {
		return StatusPage{}, mapErr(err)
	}
	if monCount != pos {
		return StatusPage{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE status_pages SET updated_at = now() WHERE id = $1`, pageID); err != nil {
		return StatusPage{}, mapErr(err)
	}
	if err := mapErr(tx.Commit(ctx)); err != nil {
		return StatusPage{}, err
	}
	return s.GetStatusPage(ctx, pageID)
}
