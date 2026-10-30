package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/badge"
	"github.com/Ariazonaa/statwarden/internal/deprecation"
	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/maintenance"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// heatmapDays is how many daily uptime bars the public page shows for the
// default 90-day range.
const heatmapDays = 90

// How many resolved incidents the public page carries.
const (
	// recentIncidentCount is the plain "what happened lately" list.
	recentIncidentCount = 10
	// maxPublicIncidents is the ceiling once older incidents with a published
	// post-mortem are added back in. It bounds a payload that anonymous
	// visitors fetch, on an instance with years of history.
	maxPublicIncidents = 25
)

// Selectable uptime ranges for the public page and the default when the query
// omits (or garbles) ?range=.
const (
	Range24h           = "24h"
	Range7d            = "7d"
	Range30d           = "30d"
	Range90d           = "90d"
	Range1y            = "1y"
	defaultPublicRange = Range90d
)

// Bucket unit labels echoed to the client so it can format bar labels for the
// chosen range without re-deriving the granularity.
const (
	bucketHour  = "hour"
	bucketDay   = "day"
	bucketMonth = "month"
)

// Password-protected pages hand a visitor a short-lived, page-scoped unlock
// cookie the first time they submit the correct password; every public output
// channel for that page (JSON, badge) then accepts the cookie, so the password
// is asked for once rather than on every request and channel.
const (
	pageUnlockCookie = "statwarden_page_unlock"
	pageUnlockTTL    = 12 * time.Hour

	// publicStatusPrefix is the URL prefix the unlock cookie is scoped to.
	publicStatusPrefix = "/api/v1/public/status"
)

// Overall status-page banner states.
const (
	OverallOperational = "operational"
	OverallDegraded    = "degraded"
	OverallMajorOutage = "major_outage"
	OverallMaintenance = "maintenance"
)

// PublicDayBar is one bucket of a monitor's uptime timeline. For the default
// 90-day range it is one day, but a selectable range buckets it hourly (24h/7d)
// or monthly (1y) instead — the page-level Bucket unit says which. Start is the
// bucket's boundary and the unambiguous key/label for any granularity; Date is
// kept as its UTC day for clients written against the original daily heatmap.
type PublicDayBar struct {
	Start  time.Time `json:"start"` // bucket start (RFC3339, UTC)
	Date   string    `json:"date"`  // YYYY-MM-DD (UTC) of the bucket start
	Status string    `json:"status"`
	Uptime float64   `json:"uptime"` // 0..1, -1 when no data
}

// PublicMonitorView is one monitor as shown on a public status page.
type PublicMonitorView struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	ShowUptime bool           `json:"show_uptime"`
	Uptime     float64        `json:"uptime"` // 0..1 over the window, -1 unknown
	Heatmap    []PublicDayBar `json:"heatmap"`
}

// PublicGroupView is a named block of monitors.
type PublicGroupView struct {
	// ID is carried so the client can key a rendered list on something
	// guaranteed unique. Group names are not: nothing dedupes them on save, so
	// two groups on one page may share a name.
	ID       int64               `json:"id"`
	Name     string              `json:"name"`
	Monitors []PublicMonitorView `json:"monitors"`
}

// PublicPageView is the full public rendering payload for a status page.
type PublicPageView struct {
	Slug            string              `json:"slug"`
	Title           string              `json:"title"`
	Description     string              `json:"description"`
	FooterText      string              `json:"footer_text"`
	Theme           string              `json:"theme"`
	LogoPath        string              `json:"logo_path,omitempty"`
	AccentColor     string              `json:"accent_color,omitempty"`
	Locale          string              `json:"locale,omitempty"`
	AllowIndexing   bool                `json:"allow_indexing"`
	OverallStatus   string              `json:"overall_status"`
	Groups          []PublicGroupView   `json:"groups"`
	Ungrouped       []PublicMonitorView `json:"ungrouped"`
	ActiveIncidents []Incident          `json:"active_incidents"`
	RecentIncidents []Incident          `json:"recent_incidents"`
	Maintenance     []PublicMaintenance `json:"maintenance"`
	// Range is the resolved uptime window (one of 24h/7d/30d/90d/1y) and Bucket
	// the granularity every monitor's bars are aggregated to (hour/day/month).
	// The client renders and labels the bars against these two, so a request
	// that omitted ?range= still sees which window it got.
	Range       string    `json:"range"`
	Bucket      string    `json:"bucket"`
	GeneratedAt time.Time `json:"generated_at"`
}

// PublicMaintenance is one maintenance window as shown to anonymous visitors:
// the subset of the full window that is safe to publish, plus Active telling
// the client whether now falls inside it. Only windows that are in effect now
// or still ahead, and that cover a monitor on this page, are ever included.
type PublicMaintenance struct {
	ID         int64      `json:"id"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	StartsAt   *time.Time `json:"starts_at,omitempty"` // RFC3339, UTC; absent for an unbounded series
	EndsAt     *time.Time `json:"ends_at,omitempty"`   // RFC3339, UTC; absent when open-ended
	Active     bool       `json:"active"`              // now is within the window
	MonitorIDs []int64    `json:"monitor_ids"`         // windows scoped to these monitors ([] = all)
}

// resolvePublicPage finds the page for a request: an explicit {slug} path param,
// else the request Host header mapped to a custom domain.
func (s *server) resolvePublicPage(r *http.Request) (StatusPage, bool) {
	if slug := chi.URLParam(r, "slug"); slug != "" {
		p, err := s.store.GetStatusPageBySlug(r.Context(), slug)
		if err != nil {
			return StatusPage{}, false
		}
		return p, true
	}
	host := r.Host
	if h, ok := splitHostPort(host); ok {
		host = h
	}
	p, err := s.store.GetStatusPageByDomain(r.Context(), host)
	if err != nil {
		return StatusPage{}, false
	}
	return p, true
}

// splitHostPort strips a trailing :port from a host, returning (host, true) when
// a port was present.
func splitHostPort(host string) (string, bool) {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i], true
	}
	return host, false
}

// checkPagePassword enforces per-page password protection across every public
// output channel. It returns true when access is granted; otherwise it has
// already written a 401 response carrying the X-Password-Required marker.
//
// Access is granted three ways, cheapest first: an unprotected page, a live
// unlock cookie from a previous password submission, or a correct password in
// this request (query param or X-Page-Password header). A correct password also
// mints the unlock cookie, so the caller's subsequent requests to the page's
// JSON and badge endpoints no longer need to carry it.
func (s *server) checkPagePassword(w http.ResponseWriter, r *http.Request, p StatusPage) bool {
	if p.PasswordHash == "" {
		return true
	}
	if c, err := r.Cookie(pageUnlockCookie); err == nil && validPageUnlock(p, c.Value, s.now()) {
		return true
	}
	supplied := r.URL.Query().Get("password")
	if supplied != "" {
		// Deprecated. A password in the query string ends up in browser history,
		// reverse-proxy access logs, referrer headers and anything that samples
		// URLs — none of which the visitor controls or can clear. It is still
		// accepted so existing links keep working, but every use is logged so an
		// operator can find the callers and move them to the header. This is a
		// public endpoint, so the log line names the page, never the password.
		s.logger.Warn("deprecated: status-page password supplied as a ?password= query parameter; use the X-Page-Password header",
			"slug", p.Slug, "ip", clientIP(r))
		// The log line only reaches the operator. The headers reach the CALLER,
		// which is the party that has to change — often somebody who embedded the
		// link and will never read this instance's logs.
		deprecation.Mark(w.Header())
	}
	if supplied == "" {
		supplied = r.Header.Get("X-Page-Password")
	}
	if supplied != "" {
		if ok, _ := auth.VerifyPassword(supplied, p.PasswordHash); ok {
			s.setPageUnlockCookie(w, r, p)
			return true
		}
	}
	w.Header().Set("X-Password-Required", "true")
	writeProblemDetail(w, http.StatusUnauthorized, "password required")
	return false
}

// setPageUnlockCookie issues the scoped, HttpOnly unlock cookie for p.
//
// The path follows the URL space the visitor actually browses. A page addressed
// by {slug} gets the slug-scoped path, so two protected pages on one host never
// clobber each other's cookie. A page addressed by CUSTOM DOMAIN is served from
// the bare prefix — its URL carries no slug at all — so a slug-scoped cookie
// would never be sent back and the visitor was asked for the password on every
// single request. One host serves one custom-domain page, so the broader path
// cannot collide there; and the token is HMAC-bound to the slug either way, so
// a cookie minted for one page cannot unlock another.
func (s *server) setPageUnlockCookie(w http.ResponseWriter, r *http.Request, p StatusPage) {
	path := publicStatusPrefix
	if chi.URLParam(r, "slug") != "" {
		path += "/" + p.Slug
	}
	exp := s.now().Add(pageUnlockTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     pageUnlockCookie,
		Value:    pageUnlockToken(p, exp),
		Path:     path,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(pageUnlockTTL.Seconds()),
	})
}

// pageUnlockToken derives a stateless unlock token: the expiry (unix seconds)
// and an HMAC over the page slug and that expiry, keyed on the page's own
// password hash. Keying on the hash means rotating or clearing the password
// silently invalidates every outstanding token; naming the slug means a token
// minted for one page cannot unlock another. No server-side state is kept.
func pageUnlockToken(p StatusPage, exp int64) string {
	payload := strconv.FormatInt(exp, 10)
	return payload + "." + pageUnlockSig(p, payload)
}

// validPageUnlock reports whether token is a live, well-formed unlock token for
// p, using a constant-time comparison so a forged signature cannot be probed
// byte by byte.
func validPageUnlock(p StatusPage, token string, now time.Time) bool {
	if p.PasswordHash == "" {
		return false
	}
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		return false
	}
	payload, sig := token[:dot], token[dot+1:]
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || now.Unix() >= exp {
		return false
	}
	want := pageUnlockSig(p, payload)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

// pageUnlockSig is the shared HMAC-SHA256 of "slug|payload" under the page's
// password hash, base64url-encoded.
func pageUnlockSig(p StatusPage, payload string) string {
	mac := hmac.New(sha256.New, []byte(p.PasswordHash))
	mac.Write([]byte(p.Slug + "|" + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// handlePublicStatusPage renders a status page for anonymous viewers.
func (s *server) handlePublicStatusPage(w http.ResponseWriter, r *http.Request) {
	page, ok := s.resolvePublicPage(r)
	if !ok || !page.Published {
		writeProblemDetail(w, http.StatusNotFound, "status page not found")
		return
	}
	if !s.checkPagePassword(w, r, page) {
		return
	}
	// Best-effort view counter (never blocks rendering).
	_ = s.store.IncrementStatusPageViews(r.Context(), page.ID)

	view, err := s.buildPublicPage(r.Context(), page, parsePublicRange(r.URL.Query().Get("range")))
	if err != nil {
		// 503, not a page.
		//
		// Every counter this view derives its banner from is a COUNT, so a read
		// that fails leaves them all at zero — and zero down out of zero monitors
		// is, by definition, "All systems operational". A pool that saturated
		// after the page row was fetched therefore rendered a green banner, an
		// empty component list and a green badge, with HTTP 200. A status page
		// reporting perfect health BECAUSE its own database is unreachable is the
		// one output this endpoint must never produce.
		// A visitor who closes the tab mid-load cancels the request context, and
		// pgx surfaces that as an ordinary read error. Logging it at ERROR would
		// make the most-visited endpoint in the app the noisiest one, on exactly
		// the event an operator would alert on. The 503 is written to a socket
		// nobody is reading either way.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.logger.Debug("public status page: request cancelled", "slug", page.Slug)
			return
		}
		s.logger.Error("public status page: store read failed", "slug", page.Slug, "error", err)
		writeProblemDetail(w, http.StatusServiceUnavailable,
			"the status page is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// parsePublicRange maps the ?range= query value to a canonical range token,
// falling back to the default for an empty or unrecognised value so a bad
// query never fails the whole page.
func parsePublicRange(v string) string {
	switch v {
	case Range24h, Range7d, Range30d, Range90d, Range1y:
		return v
	default:
		return defaultPublicRange
	}
}

// buildPublicPage assembles the full public rendering payload for a page,
// including each monitor's uptime bars bucketed to the requested range.
// rangeToken must already be canonicalised (see parsePublicRange).
func (s *server) buildPublicPage(ctx context.Context, page StatusPage, rangeToken string) (PublicPageView, error) {
	return s.buildPublicPageView(ctx, page, rangeToken, true)
}

// buildPublicPageView is buildPublicPage with the uptime bars made optional.
// withBars=false is for consumers that only need the banner state: the badge
// endpoint used to build the full heatmap — every bucket for every monitor —
// and then read nothing but OverallStatus off it. Badges are embedded in
// READMEs and dashboards, so that was the highest-volume caller doing the most
// throwaway work.
func (s *server) buildPublicPageView(ctx context.Context, page StatusPage, rangeToken string, withBars bool) (PublicPageView, error) {
	now := s.now()
	buckets, unit, from := publicBuckets(rangeToken, now)
	if !withBars {
		buckets = nil
	}
	view := PublicPageView{
		Slug:        page.Slug,
		Title:       page.Title,
		Description: page.Description,
		FooterText:  page.FooterText,
		Theme:       page.Theme,
		// logo_path was free client text before the upload endpoints existed, so
		// legacy rows may hold an arbitrary external URL. The public page
		// interpolates this into an <img src>, so sanitise on read rather than
		// trusting the column.
		LogoPath:      sanitizeLogoPath(page.LogoPath),
		AccentColor:   page.AccentColor,
		Locale:        page.Locale,
		AllowIndexing: page.AllowIndexing,
		Range:         rangeToken,
		Bucket:        unit,
		GeneratedAt:   now,

		// Start every collection non-nil. encoding/json renders a nil slice as
		// null, and the public page reads .length on all five without guarding,
		// so a page with no groups or no incidents rendered a blank screen.
		// writeJSON only normalises a nil slice at the top level of a response;
		// these are fields inside a struct, so they must be initialised here.
		Groups:          []PublicGroupView{},
		Ungrouped:       []PublicMonitorView{},
		ActiveIncidents: []Incident{},
		RecentIncidents: []Incident{},
		Maintenance:     []PublicMaintenance{},
	}

	// Build monitor views, grouped.
	monViews := map[int64]PublicMonitorView{}
	var downCount, total, maintCount int
	for _, e := range page.Monitors {
		// A placement whose monitor cannot be read is SKIPPED, not rendered.
		//
		// This became reachable when deleting a monitor turned into a soft delete
		// (migration 0018): the status_page_monitors row survives the delete, and
		// buildMonitorView's "if GetMonitor succeeded" was a silent fallback that
		// produced a nameless row stuck on "pending" — a component the visitor
		// cannot identify, dragging the page's overall status with it. It is also
		// the right answer for a genuinely missing row.
		mv, st, ok, err := s.buildMonitorView(ctx, e, now, buckets, from)
		if err != nil {
			// A store failure is NOT a missing monitor. Both used to arrive here
			// as ok=false and were skipped identically, which is how a database
			// problem turned into an empty, healthy-looking page.
			return PublicPageView{}, err
		}
		if !ok {
			continue
		}
		monViews[e.MonitorID] = mv
		total++
		switch st {
		case domain.StatusDown:
			downCount++
		case domain.StatusMaintenance:
			maintCount++
		}
	}

	// Assemble into groups (in group order) then ungrouped.
	groupByID := map[int64]*PublicGroupView{}
	for i := range page.Groups {
		g := page.Groups[i]
		// Monitors starts EMPTY, never nil. A group with nothing in it is an
		// ordinary state — created, or emptied — and json.Marshal renders a nil
		// slice as null. The public page then iterates it: `{#each group.monitors}`
		// and a flatMap over every group, with the TypeScript type declaring
		// `monitors: PublicMonitorView[]` and no nullable in sight. So one empty
		// group did not degrade the page, it broke the render — and only against
		// the real server, because the in-browser mock answers [].
		//
		// The five top-level collections above are initialised for exactly this
		// reason; this one is nested a level deeper and was missed.
		gv := &PublicGroupView{ID: g.ID, Name: g.Name, Monitors: []PublicMonitorView{}}
		groupByID[g.ID] = gv
	}
	for _, e := range page.Monitors {
		// The comma-ok is the whole point, and leaving it off cost a blank public
		// page for every visitor.
		//
		// The loop above skips a placement whose monitor cannot be read — a
		// monitor in the recycle bin, since deleting one stopped removing the
		// status_page_monitors row. This loop walks page.Monitors AGAIN, so a
		// plain map index handed back the ZERO PublicMonitorView for exactly those
		// placements and appended it as a component: id 0, no name, no status, and
		// a nil Heatmap that marshals as `null`. The page then did
		// `m.heatmap.map(...)` on null and died, and two binned monitors gave two
		// rows keyed 0 and tripped Svelte's duplicate-key throw.
		//
		// The badge endpoint reads only OverallStatus, which comes from the first
		// loop — so a badge-based canary stayed green while the page was dead.
		mv, ok := monViews[e.MonitorID]
		if !ok {
			continue
		}
		if e.GroupID != nil {
			if gv := groupByID[*e.GroupID]; gv != nil {
				gv.Monitors = append(gv.Monitors, mv)
				continue
			}
		}
		view.Ungrouped = append(view.Ungrouped, mv)
	}
	for i := range page.Groups {
		gv := groupByID[page.Groups[i].ID]
		// An EMPTY group is not rendered.
		//
		// A heading with nothing under it says nothing to a visitor, and it is
		// reachable in a way the operator cannot fix from the editor: a group
		// whose only member is now in the recycle bin keeps existing, because the
		// placement survives a soft delete on purpose, but the monitor is skipped
		// on the way out. The editor never shows that placement — it maps over
		// live monitors — so the group cannot be emptied or renamed from the UI
		// either. It would simply sit on the public page, permanently, with no
		// way to remove it short of purging the monitor.
		if gv == nil || len(gv.Monitors) == 0 {
			continue
		}
		view.Groups = append(view.Groups, *gv)
	}

	// Incidents split into active (unresolved) and recent (resolved).
	//
	// Each goes through publicCopy on the way in. This payload is served to
	// anonymous visitors and embeds the Incident type whole, so a field added to
	// that struct is published by default — which is the wrong default for an
	// unpublished post-mortem draft. Stripping happens here and in
	// announceIncident, the only two places an incident reaches the public.
	incidents, err := s.store.ListIncidents(ctx, page.ID)
	if err != nil {
		// Discarding this meant a page whose incident table was unreadable
		// announced that there were no incidents.
		return PublicPageView{}, err
	}
	for _, in := range incidents {
		if in.Status != IncidentResolved {
			view.ActiveIncidents = append(view.ActiveIncidents, in.publicCopy())
			continue
		}
		// The ten most recent, plus any older one carrying a PUBLISHED
		// retrospective, up to a hard ceiling.
		//
		// A published post-mortem is a document somebody deliberately put on this
		// page, usually the one they link to afterwards. Dropping it because ten
		// unrelated incidents have happened since made the link dead with no
		// redirect and no note — the write-up was still stored, still marked
		// published, and simply not rendered anywhere. The ceiling keeps the
		// payload bounded on an instance with years of them.
		switch {
		case len(view.RecentIncidents) < recentIncidentCount:
		case in.PostmortemPublishedAt != nil && len(view.RecentIncidents) < maxPublicIncidents:
		default:
			continue
		}
		view.RecentIncidents = append(view.RecentIncidents, in.publicCopy())
	}
	hasCritical := false
	for _, in := range view.ActiveIncidents {
		if in.Severity == "critical" || in.Severity == "major" {
			hasCritical = true
		}
	}

	// In-effect / upcoming maintenance windows touching this page's monitors.
	// pageMaintenance returns nil when there is nothing to report, which would
	// re-introduce the null the literal above deliberately avoids.
	mw, maintNow, err := s.pageMaintenance(ctx, page, now)
	if err != nil {
		return PublicPageView{}, err
	}
	if mw != nil {
		view.Maintenance = mw
	}
	// Only a window that is in effect right now colours the banner. Listing a
	// window scheduled for next Tuesday used to put the whole page into
	// "maintenance" immediately.
	if maintNow {
		maintCount++
	}

	view.OverallStatus = overallStatus(downCount, total, maintCount, hasCritical, len(view.ActiveIncidents))
	return view, nil
}

// buildMonitorView computes a monitor's current status, uptime over the
// selected window and one bar per bucket of that window.
//
// The third result is false when the monitor GENUINELY is not there — deleted
// into the recycle bin, or gone. The fourth is set when the store could not
// answer at all. The two were one value for a while, and collapsing them is how
// an unreachable database rendered as a page with no components and a green
// banner: every skipped placement looked like an ordinary soft delete.
//
// The caller drops a not-found placement and fails the whole request on an
// error; see the loop in buildPublicPageView.
func (s *server) buildMonitorView(ctx context.Context, e StatusPageMonitorEntry, now time.Time, buckets []bucketSpan, from time.Time) (PublicMonitorView, domain.MonitorStatus, bool, error) {
	// Heatmap starts EMPTY, never nil, for the reason every other collection on
	// this payload does: encoding/json renders a nil slice as null, and
	// emptySliceAsArray only normalises the TOP level of a response, never a
	// nested struct field. The client's PublicMonitorView declares
	// `heatmap: PublicDayBar[]` with no nullable in sight and calls .map() on it
	// unguarded. Without this the field is null whenever the reporter is absent —
	// which is DEV_MODE against the real server — and the page dies there too.
	mv := PublicMonitorView{
		ID: e.MonitorID, ShowUptime: e.ShowUptime, Uptime: -1,
		Heatmap: []PublicDayBar{},
	}
	m, err := s.store.GetMonitor(ctx, e.MonitorID)
	if errors.Is(err, ErrNotFound) {
		return PublicMonitorView{}, domain.StatusPending, false, nil
	}
	if err != nil {
		return PublicMonitorView{}, domain.StatusPending, false, err
	}
	mv.Name = m.Name
	status := m.Status
	if s.engine != nil {
		if st, ok := s.engine.StatusOf(e.MonitorID); ok {
			status = st
		}
	}
	mv.Status = string(status)

	// show_uptime=false means the figures are NOT PUBLISHED, not merely hidden.
	//
	// Both were computed unconditionally and the flag was only echoed for the
	// client to act on — so an anonymous curl of the JSON got the uptime and all
	// ninety daily bars for a monitor the operator had marked as not-for-
	// publication, and the page itself drew the bar strip regardless (only the
	// percentage text was gated), with a per-day percentage in every tooltip.
	// The archive endpoint has always honoured the flag; this is the half that
	// leaked.
	//
	// Skipping the work also removes the per-monitor reporter call for a monitor
	// whose numbers are then discarded.
	// withBars=false is the badge, which reads nothing but OverallStatus — so the
	// per-monitor uptime is skipped too, not just the bars. Gating only the bars
	// left the expensive half in place: report.Service.Uptime walks its range a
	// day at a time looking for rollup gaps, once per monitor, on the highest
	// volume unauthenticated endpoint in the app, and every result was discarded.
	if s.reporter != nil && e.ShowUptime && len(buckets) > 0 {
		// Overall uptime over the whole selected window, so the headline figure
		// tracks the range the visitor picked rather than a fixed 90 days.
		if res, err := s.reporter.Uptime(ctx, e.MonitorID, report.Range{From: from, To: now}); err == nil {
			mv.Uptime = res.Uptime
		}
		bars, err := s.buildBars(ctx, e.MonitorID, buckets)
		if err != nil {
			// The last swallowed read on this path. When the ROLLUP query is the
			// one that fails, an all-"none" heatmap came back with HTTP 200 — and
			// the archive then renders "Nothing to archive yet. This page
			// publishes no availability figures." That is the same sentence the
			// rest of this fix exists to stop the page saying.
			return PublicMonitorView{}, domain.StatusPending, false, err
		}
		mv.Heatmap = bars
	}
	return mv, status, true, nil
}

// buildBars computes one uptime bar per bucket (oldest first) in a single
// reporter call. It used to issue one query per bucket, which on the default
// 90-day range meant 90 sequential queries per monitor — times every monitor on
// the page, on a public endpoint. UptimeSeries answers the whole run from the
// rollups instead, choosing granularity from the bucket width.
func (s *server) buildBars(ctx context.Context, monitorID int64, buckets []bucketSpan) ([]PublicDayBar, error) {
	bars := make([]PublicDayBar, 0, len(buckets))
	if len(buckets) == 0 {
		return bars, nil // bars were not requested (badge path): ask the store nothing
	}
	for _, b := range buckets {
		bars = append(bars, PublicDayBar{
			Start: b.start, Date: b.start.Format("2006-01-02"), Uptime: -1, Status: "none",
		})
	}
	ranges := make([]report.Range, 0, len(buckets))
	for _, b := range buckets {
		ranges = append(ranges, report.Range{From: b.start, To: b.end})
	}
	results, err := s.reporter.UptimeSeries(ctx, monitorID, ranges)
	if err != nil {
		// Reported, not swallowed. All-"none" bars are indistinguishable from a
		// monitor that genuinely has no history, and the archive renders that as
		// "This page publishes no availability figures" — an assertion about data
		// the server never managed to read.
		return nil, err
	}
	for i, res := range results {
		if i >= len(bars) || res.Total == 0 {
			continue
		}
		bars[i].Uptime = res.Uptime
		switch {
		case res.Uptime >= 0.9999:
			bars[i].Status = "up"
		case res.Uptime >= 0.95:
			bars[i].Status = "degraded"
		default:
			bars[i].Status = "down"
		}
	}
	return bars, nil
}

// meanUptime averages the buckets that hold data, or -1 when none do.
//
// This is deliberately NOT Service.Uptime over the whole archived window, and
// the difference is not cosmetic: that call walks its range ONE DAY AT A TIME
// looking for rollup gaps (see the loop in report.Service.Uptime), which is
// precisely why parseReportRange refuses a report window wider than ~400 days.
// The archive spans up to ten years, on an UNAUTHENTICATED endpoint, once per
// monitor — so calling it here reintroduced, on the public surface, the exact
// full-scan the report endpoints are capped to avoid.
//
// The buckets are already in hand and already cost one batched query, so the
// column is computed from them instead. It is an unweighted mean over months
// rather than a sample-weighted one — the bars carry a fraction, not a count —
// which is why the page says so under the table.
func meanUptime(bars []PublicDayBar) float64 {
	sum, n := 0.0, 0
	for _, b := range bars {
		if b.Uptime < 0 {
			continue
		}
		sum += b.Uptime
		n++
	}
	if n == 0 {
		return -1
	}
	return sum / float64(n)
}

// bucketSpan is one uptime bar's half-open window [start, end).
type bucketSpan struct {
	start time.Time
	end   time.Time
}

// publicBuckets expands a canonical range token into the ordered list of bar
// windows (oldest first), the bucket unit label, and the window start used for
// the monitor's overall uptime. Boundaries are aligned in UTC — hourly for
// 24h/7d, daily for 30d/90d, monthly for 1y — and the final bucket is the
// current, still-open period, matching how the original daily heatmap included
// today. An unknown token is treated as the default range.
func publicBuckets(rangeToken string, now time.Time) (buckets []bucketSpan, unit string, from time.Time) {
	u := now.UTC()
	switch rangeToken {
	case Range24h, Range7d:
		unit = bucketHour
		hours := 24
		if rangeToken == Range7d {
			hours = 24 * 7
		}
		hourStart := time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
		buckets = make([]bucketSpan, 0, hours)
		for i := hours - 1; i >= 0; i-- {
			start := hourStart.Add(time.Duration(-i) * time.Hour)
			buckets = append(buckets, bucketSpan{start: start, end: start.Add(time.Hour)})
		}
	case Range1y:
		unit = bucketMonth
		const months = 12
		monthStart := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		buckets = make([]bucketSpan, 0, months)
		for i := months - 1; i >= 0; i-- {
			start := monthStart.AddDate(0, -i, 0)
			buckets = append(buckets, bucketSpan{start: start, end: start.AddDate(0, 1, 0)})
		}
	default: // Range30d, Range90d and the default
		unit = bucketDay
		days := heatmapDays
		if rangeToken == Range30d {
			days = 30
		}
		dayStart := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
		buckets = make([]bucketSpan, 0, days)
		for i := days - 1; i >= 0; i-- {
			start := dayStart.AddDate(0, 0, -i)
			buckets = append(buckets, bucketSpan{start: start, end: start.AddDate(0, 0, 1)})
		}
	}
	if len(buckets) > 0 {
		from = buckets[0].start
	} else {
		from = u
	}
	return buckets, unit, from
}

// pageMaintenance returns the in-effect and upcoming maintenance windows
// affecting any of the page's monitors, soonest first, and reports whether any
// of them is in effect right now.
//
// Whether a window is IN EFFECT is not decided here: it is asked of the shared
// evaluator, the same instance the engine suppresses alerts with. The old
// ends_at test could not see a recurring window at all (it is in effect on some
// days and not on others, and may carry no ends_at), so the page announced
// maintenance while the engine was still alerting — and vice versa.
func (s *server) pageMaintenance(ctx context.Context, page StatusPage, now time.Time) ([]PublicMaintenance, bool, error) {
	all, err := s.store.ListMaintenance(ctx)
	if err != nil {
		// Reported, not swallowed: "no windows" and "I could not read the
		// windows" are different answers, and only one of them is safe to render
		// as an ordinary page.
		return nil, false, err
	}
	inEffect := map[int64]struct{}{}
	if s.maint != nil {
		for _, w := range s.maint.ActiveAt(now) {
			inEffect[w.ID] = struct{}{}
		}
	}
	pageMons := map[int64]struct{}{}
	for _, id := range page.MonitorIDs {
		pageMons[id] = struct{}{}
	}
	var out []PublicMaintenance
	anyInEffect := false
	for _, mw := range all {
		if !mw.Active {
			continue
		}
		_, active := inEffect[mw.ID]
		if !active && !upcoming(mw, now) {
			continue
		}
		touches := mw.AppliesToAll
		for _, id := range mw.MonitorIDs {
			if _, ok := pageMons[id]; ok {
				touches = true
				break
			}
		}
		if !touches {
			continue
		}
		out = append(out, toPublicMaintenance(mw, active))
		anyInEffect = anyInEffect || active
	}
	sort.Slice(out, func(i, j int) bool {
		return startOrZero(out[i].StartsAt).Before(startOrZero(out[j].StartsAt))
	})
	return out, anyInEffect, nil
}

// toPublicMaintenance narrows a stored window to the visitor-facing subset,
// stamping whether it is in effect right now. Recurrence, timezone and audit
// timestamps are intentionally dropped: the public page only announces that a
// window exists, when it runs and which monitors it covers. monitor_ids is
// forced non-nil so the client can iterate it without a guard; an empty slice
// (the window covers every monitor) stays an empty array, not null.
func toPublicMaintenance(mw MaintenanceWindow, active bool) PublicMaintenance {
	ids := mw.MonitorIDs
	if ids == nil {
		ids = []int64{}
	}
	return PublicMaintenance{
		ID:         mw.ID,
		Title:      mw.Title,
		Body:       mw.Description,
		StartsAt:   mw.StartsAt,
		EndsAt:     mw.EndsAt,
		Active:     active,
		MonitorIDs: ids,
	}
}

// upcoming reports whether a window that is not currently in effect still has
// an occurrence ahead of it, and so is worth announcing. A recurring series
// recurs until its ends_at (if any) passes; a one-off is upcoming only until it
// starts, after which the evaluator alone decides.
func upcoming(mw MaintenanceWindow, now time.Time) bool {
	if mw.EndsAt != nil && !mw.EndsAt.After(now) {
		return false
	}
	if mw.Strategy == maintenance.StrategyRecurring {
		return true
	}
	return mw.StartsAt != nil && mw.StartsAt.After(now)
}

func startOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// overallStatus maps monitor/incident counts to a banner state.
func overallStatus(down, total, maint int, hasCritical bool, activeIncidents int) string {
	switch {
	case down > 0 && (hasCritical || (total > 0 && down == total)):
		return OverallMajorOutage
	case down > 0 || activeIncidents > 0:
		return OverallDegraded
	case maint > 0:
		return OverallMaintenance
	default:
		return OverallOperational
	}
}

// handlePublicPageBadge renders the page's overall-status SVG badge. Public.
func (s *server) handlePublicPageBadge(w http.ResponseWriter, r *http.Request) {
	page, ok := s.resolvePublicPage(r)
	if !ok || !page.Published {
		writeProblemDetail(w, http.StatusNotFound, "status page not found")
		return
	}
	if !s.checkPagePassword(w, r, page) {
		return
	}
	// The badge only needs the overall banner state, so it is built WITHOUT the
	// uptime bars: rendering them meant a full heatmap per monitor (90 buckets on
	// the default range) whose every value was then discarded.
	view, err := s.buildPublicPageView(r.Context(), page, defaultPublicRange, false)
	label := r.URL.Query().Get("label")
	if label == "" {
		label = page.Title
	}
	if err != nil {
		// A badge is pasted into a README and glanced at; painting it green
		// because the database is unreachable is worse than painting nothing.
		// "unknown" is the honest answer and is visually distinct from "up".
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.logger.Debug("status page badge: request cancelled", "slug", page.Slug)
			return
		}
		s.logger.Error("status page badge: store read failed", "slug", page.Slug, "error", err)
		writeSVG(w, badge.Status(label, "unknown"))
		return
	}
	// Map overall status to a badge status token the badge package understands.
	badgeStatus := "up"
	switch view.OverallStatus {
	case OverallMajorOutage:
		badgeStatus = "down"
	case OverallDegraded:
		badgeStatus = "pending"
	case OverallMaintenance:
		badgeStatus = "maintenance"
	}
	writeSVG(w, badge.Status(label, badgeStatus))
}

// --- availability archive ---

// Archive bounds.
const (
	// defaultArchiveMonths is the window the archive page opens with: two years
	// reads as history without being a wall.
	defaultArchiveMonths = 24
	// maxArchiveMonths caps ?months=. Each month is one bucket per monitor, and
	// the whole run goes to the reporter in one call, so the cost is linear —
	// but this is an UNAUTHENTICATED endpoint, so it needs a ceiling that is not
	// "whatever the caller typed". Ten years is past any instance's data.
	maxArchiveMonths = 120
)

// PublicHistoryView is the availability archive: one uptime figure per month per
// monitor, oldest month first.
//
// It carries the page's branding as well, so the archive renders like the status
// page it belongs to without a second request. The uptime bars reuse
// PublicDayBar because they are the same thing at a coarser granularity — its
// `start` field is documented as the unambiguous key for any bucket unit — which
// also means the client's existing bar mapper works unchanged.
type PublicHistoryView struct {
	Slug          string    `json:"slug"`
	Title         string    `json:"title"`
	Theme         string    `json:"theme"`
	LogoPath      string    `json:"logo_path,omitempty"`
	AccentColor   string    `json:"accent_color,omitempty"`
	Locale        string    `json:"locale,omitempty"`
	AllowIndexing bool      `json:"allow_indexing"`
	GeneratedAt   time.Time `json:"generated_at"`

	// Months are the bucket starts (UTC, first of the month), oldest first. Every
	// monitor's Months slice is parallel to this one, so the client can render a
	// table without matching timestamps.
	Months []time.Time `json:"months"`
	// Overall is the page-wide availability per month, parallel to Months: the
	// mean over the monitors that have data that month, or -1 when none do.
	Overall  []float64              `json:"overall"`
	Monitors []PublicHistoryMonitor `json:"monitors"`
}

// PublicHistoryMonitor is one monitor's row in the archive.
type PublicHistoryMonitor struct {
	ID     int64          `json:"id"`
	Name   string         `json:"name"`
	Months []PublicDayBar `json:"months"`
	// Uptime is the figure over the whole archived window, or -1 with no data.
	Uptime float64 `json:"uptime"`
}

// monthlyBuckets returns the last n calendar months as buckets, oldest first,
// aligned in UTC. The final bucket is the current, still-running month —
// matching publicBuckets, where the last bar is always the open period.
func monthlyBuckets(now time.Time, n int) []bucketSpan {
	u := now.UTC()
	monthStart := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]bucketSpan, 0, n)
	for i := n - 1; i >= 0; i-- {
		start := monthStart.AddDate(0, -i, 0)
		out = append(out, bucketSpan{start: start, end: start.AddDate(0, 1, 0)})
	}
	return out
}

// parseArchiveMonths reads ?months=, clamped rather than rejected.
//
// Clamping is right here and wrong on the audit export, and the difference is
// what the parameter means. There, a window is the caller's statement about
// which evidence they want and guessing corrupts it; here it is a zoom level on
// a public page, and answering a nonsensical one with a 400 turns a stray link
// into an error screen for a visitor who cannot fix it.
func parseArchiveMonths(raw string) int {
	if raw == "" {
		return defaultArchiveMonths
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return defaultArchiveMonths
	}
	if n > maxArchiveMonths {
		return maxArchiveMonths
	}
	return n
}

// handlePublicStatusHistory serves the availability archive for a status page.
//
// It is reachable only as /public/status/{slug}/history, never as a bare
// /public/status/history for the custom-domain form the main page supports. That
// route would be a static segment competing with {slug}, so a page whose slug is
// literally "history" would become unreachable — and the client does not need
// it: the page payload carries its own slug, so a visitor on a custom domain
// still knows which one to ask for.
func (s *server) handlePublicStatusHistory(w http.ResponseWriter, r *http.Request) {
	page, ok := s.resolvePublicPage(r)
	if !ok || !page.Published {
		writeProblemDetail(w, http.StatusNotFound, "status page not found")
		return
	}
	// The same password gate as the page itself. Without it the archive would be
	// a way around the password: same data, coarser granularity.
	if !s.checkPagePassword(w, r, page) {
		return
	}
	view, err := s.buildHistoryView(r.Context(), page, parseArchiveMonths(r.URL.Query().Get("months")))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.logger.Debug("status page archive: request cancelled", "slug", page.Slug)
			return
		}
		s.logger.Error("status page archive: store read failed", "slug", page.Slug, "error", err)
		writeProblemDetail(w, http.StatusServiceUnavailable,
			"the availability archive is temporarily unavailable")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// buildHistoryView assembles the archive payload.
//
// It returns an error for the same reason buildPublicPageView does: a store
// failure that is swallowed here comes out as an empty archive, which the client
// renders as "Nothing to archive yet. This page publishes no availability
// figures." — a public assertion about data the server never managed to read.
func (s *server) buildHistoryView(ctx context.Context, page StatusPage, months int) (PublicHistoryView, error) {
	now := s.now()
	buckets := monthlyBuckets(now, months)

	view := PublicHistoryView{
		Slug:          page.Slug,
		Title:         page.Title,
		Theme:         page.Theme,
		LogoPath:      sanitizeLogoPath(page.LogoPath),
		AccentColor:   page.AccentColor,
		Locale:        page.Locale,
		AllowIndexing: page.AllowIndexing,
		GeneratedAt:   now,
		// Non-nil for the reason every other collection on the public payload is:
		// encoding/json renders a nil slice as null, and the client reads .length
		// on all three without guarding.
		Months:   make([]time.Time, 0, len(buckets)),
		Overall:  make([]float64, 0, len(buckets)),
		Monitors: []PublicHistoryMonitor{},
	}
	for _, b := range buckets {
		view.Months = append(view.Months, b.start)
	}
	if s.reporter == nil {
		// Reporting unavailable: answer the shape with no data rather than a 503.
		// The archive is a public page, and an operator cannot act on an error a
		// visitor sees.
		for range buckets {
			view.Overall = append(view.Overall, -1)
		}
		return view, nil
	}

	for _, e := range page.Monitors {
		// show_uptime=false means the operator chose not to publish this
		// monitor's figures. An archive that is nothing BUT figures must honour
		// that, or the setting is a display quirk on one page instead of a
		// decision about what is public.
		if !e.ShowUptime {
			continue
		}
		m, err := s.store.GetMonitor(ctx, e.MonitorID)
		if errors.Is(err, ErrNotFound) {
			// Binned or gone: skip the placement, exactly as the page does.
			continue
		}
		if err != nil {
			// A store failure is not a missing monitor. Swallowing it here made
			// this route reproduce, verbatim, the bug the page endpoint was just
			// fixed for: with the pool saturated every read fails, Monitors comes
			// out empty, and the archive answers 200 — which the client renders
			// as "Nothing to archive yet. This page publishes no availability
			// figures." A public page asserting a fact it did not measure,
			// because its own database is unreachable, one route away from the
			// one that now says 503.
			return PublicHistoryView{}, err
		}
		bars, err := s.buildBars(ctx, e.MonitorID, buckets)
		if err != nil {
			return PublicHistoryView{}, err
		}
		view.Monitors = append(view.Monitors, PublicHistoryMonitor{
			ID:     e.MonitorID,
			Name:   m.Name,
			Months: bars,
			Uptime: meanUptime(bars),
		})
	}

	// Page-wide figure per month: the mean over the monitors that HAVE data that
	// month. Treating a missing month as 100% would invent availability for a
	// monitor that did not exist yet — which is exactly the number an archive
	// must not make up.
	//
	// Same rule as meanUptime uses down the rows, so the table's last column and
	// its last row are the same kind of number and the reader is not comparing
	// two differently-derived averages.
	for i := range buckets {
		sum, n := 0.0, 0
		for _, mon := range view.Monitors {
			if i < len(mon.Months) && mon.Months[i].Uptime >= 0 {
				sum += mon.Months[i].Uptime
				n++
			}
		}
		if n == 0 {
			view.Overall = append(view.Overall, -1)
			continue
		}
		view.Overall = append(view.Overall, sum/float64(n))
	}
	return view, nil
}
