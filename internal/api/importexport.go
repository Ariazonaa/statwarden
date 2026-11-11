package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/importexport"
)

// exportPageSize is how many monitors one export read pulls. The export pages
// through ALL of them: the previous single read capped at 10 000 and said
// nothing when it truncated, so a large instance silently exported a partial
// configuration that looked complete.
const exportPageSize = 500

// handleExport returns the full configuration bundle: monitors, notifications,
// tags and status pages.
//
// The bundle carries NO secrets. Monitor configs are opened and their
// credential fields replaced with the redaction placeholder; notification
// configs are reduced to a marker. An export is a file that gets copied, mailed
// and committed, so it must not hold credentials — and emitting the raw sealed
// envelope instead (what this used to do) made the bundle useless anyway, since
// importing it re-sealed ciphertext into a config nothing can open. Admin only.
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var monitors []domain.Monitor
	for offset := 0; ; offset += exportPageSize {
		page, total, err := s.store.ListMonitors(ctx, MonitorFilter{}, Page{Limit: exportPageSize, Offset: offset})
		if err != nil {
			writeProblemDetail(w, http.StatusInternalServerError, "list monitors")
			return
		}
		monitors = append(monitors, page...)
		if len(page) == 0 || len(monitors) >= total {
			break
		}
	}
	nameByID := make(map[int64]string, len(monitors))
	ids := make([]int64, 0, len(monitors))
	for _, m := range monitors {
		nameByID[m.ID] = m.Name
		ids = append(ids, m.ID)
	}
	bundle := importexport.Bundle{Version: importexport.BundleVersion, ExportedAt: s.now().UTC().Format(time.RFC3339)}

	// A monitor's tags, channels, parent and folder are part of its
	// configuration. Leaving them out made a restore come back with monitors
	// that alert nobody, and the import could not even warn about it.
	links, err := s.store.MonitorLinks(ctx, ids)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "list monitor links")
		return
	}
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "list tags")
		return
	}
	tagName := make(map[int64]string, len(tags))
	for _, t := range tags {
		tagName[t.ID] = t.Name
		bundle.Tags = append(bundle.Tags, importexport.TagSpec{Name: t.Name, Color: t.Color})
	}
	notifs, err := s.store.ListNotifications(ctx)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "list notifications")
		return
	}
	notifName := make(map[int64]string, len(notifs))
	for _, n := range notifs {
		notifName[n.ID] = n.Name
	}
	groups, err := s.store.ListMonitorGroups(ctx)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "list monitor groups")
		return
	}
	groupName := make(map[int64]string, len(groups))
	for _, g := range groups {
		groupName[g.ID] = g.Name
		bundle.MonitorGroups = append(bundle.MonitorGroups, importexport.MonitorGroupSpec{Name: g.Name, SortOrder: g.SortOrder})
	}

	for _, m := range monitors {
		spec := importexport.MonitorSpec{
			Name:            m.Name,
			Type:            m.Type,
			Config:          s.redactMonitorConfig(m.Type, s.decryptMonitorConfig(m.Config)),
			IntervalSeconds: int(m.Interval / time.Second),
			MaxRetries:      m.MaxRetries,
			RecoveryChecks:  m.RecoveryChecks,
			RetryInterval:   int(m.RetryInterval / time.Second),
			ResendInterval:  int(m.ResendInterval / time.Minute),
			UpsideDown:      m.UpsideDown,
			Active:          m.Active,
			Description:     m.Description,
			LinkURL:         m.LinkURL,
		}
		if m.Timeout > 0 {
			t := int(m.Timeout / time.Second)
			spec.Timeout = &t
		}
		for _, id := range links[m.ID].TagIDs {
			if name, ok := tagName[id]; ok {
				spec.Tags = append(spec.Tags, name)
			}
		}
		for _, id := range links[m.ID].NotificationIDs {
			if name, ok := notifName[id]; ok {
				spec.Notifications = append(spec.Notifications, name)
			}
		}
		if m.ParentID != nil {
			spec.Parent = nameByID[*m.ParentID]
		}
		if m.GroupID != nil {
			spec.Group = groupName[*m.GroupID]
		}
		bundle.Monitors = append(bundle.Monitors, spec)
	}

	for _, n := range notifs {
		bundle.Notifications = append(bundle.Notifications, importexport.NotificationSpec{
			Name: n.Name,
			Type: n.Type,
			// Same read path the API uses: a sealed config becomes the marker
			// {"$encrypted":true}. Unlike monitors there is no per-provider list of
			// secret FIELDS to redact selectively, so the whole config is withheld
			// and the channel has to be reconfigured after import — which the
			// import reports as a warning rather than failing silently.
			Config:    redactConfig(n.Config),
			IsDefault: n.IsDefault,
			Active:    n.Active,
		})
	}

	pages, err := s.store.ListStatusPages(ctx)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "list status pages")
		return
	}
	for _, p := range pages {
		spec := importexport.StatusPageSpec{
			Slug:          p.Slug,
			Title:         p.Title,
			Description:   p.Description,
			FooterText:    p.FooterText,
			Theme:         p.Theme,
			AccentColor:   p.AccentColor,
			Locale:        p.Locale,
			Published:     p.Published,
			HasPassword:   p.HasPassword || p.PasswordHash != "",
			AllowIndexing: p.AllowIndexing,
		}
		for _, mid := range p.MonitorIDs {
			if name, ok := nameByID[mid]; ok {
				spec.Monitors = append(spec.Monitors, name)
			}
		}
		groupName := make(map[int64]string, len(p.Groups))
		for _, g := range p.Groups {
			groupName[g.ID] = g.Name
			spec.Groups = append(spec.Groups, importexport.StatusPageGroupSpec{Name: g.Name, SortOrder: g.SortOrder})
		}
		for _, e := range p.Monitors {
			name, ok := nameByID[e.MonitorID]
			if !ok {
				continue
			}
			entry := importexport.StatusPageEntrySpec{Monitor: name, SortOrder: e.SortOrder, ShowUptime: e.ShowUptime}
			if e.GroupID != nil {
				entry.Group = groupName[*e.GroupID]
			}
			spec.Layout = append(spec.Layout, entry)
		}
		bundle.StatusPages = append(bundle.StatusPages, spec)
	}

	p := principalFrom(r)
	s.audit(ctx, &p.userID, "config.export", "", nil, clientIP(r), nil)
	w.Header().Set("Content-Disposition", `attachment; filename="statwarden-export.json"`)
	writeJSON(w, http.StatusOK, bundle)
}

// importResult summarises what an import created.
type importResult struct {
	Monitors      int      `json:"monitors"`
	Notifications int      `json:"notifications"`
	Tags          int      `json:"tags"`
	StatusPages   int      `json:"status_pages"`
	Warnings      []string `json:"warnings,omitempty"`
}

// handleImport restores a configuration bundle. Existing entities are left in
// place; conflicting names/slugs are skipped and reported. Admin only.
func (s *server) handleImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "read body")
		return
	}
	var bundle importexport.Bundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "invalid bundle JSON: "+err.Error())
		return
	}
	// The version is stamped on every export precisely so this check can exist,
	// and nothing read it: `Version` was written and never compared anywhere, so a
	// bundle from a newer Statwarden was imported field-by-field against the older
	// reader's idea of the format. json.Unmarshal is lenient here on purpose (a
	// bundle should survive additive changes), which is exactly what makes the
	// silent path dangerous — a spec whose meaning changed rather than its name
	// would import as plausible nonsense.
	//
	// Only a NEWER bundle is refused. Zero means the field was absent, which a
	// hand-written bundle is entitled to do, and older versions stay readable —
	// dropping compatibility is a decision for whoever changes the format.
	if bundle.Version > importexport.BundleVersion {
		writeProblemDetail(w, http.StatusBadRequest, fmt.Sprintf(
			"bundle version %d is newer than this server understands (%d); upgrade Statwarden before importing it",
			bundle.Version, importexport.BundleVersion))
		return
	}
	res := s.applyBundle(r, bundle.Monitors, bundle.Notifications, bundle.Tags, bundle.MonitorGroups, bundle.StatusPages)
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "config.import", "", nil, clientIP(r), nil)
	writeJSON(w, http.StatusOK, res)
}

// handleImportKuma maps an Uptime-Kuma export and creates the resulting
// monitors and notifications. Admin only.
func (s *server) handleImportKuma(w http.ResponseWriter, r *http.Request) {
	// A Kuma SQLite database carries heartbeat history, so it can be large;
	// allow up to 128 MiB. The legacy JSON export is tiny by comparison.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<20))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "read body")
		return
	}
	// Detect the upload: a SQLite file starts with the fixed 16-byte header
	// "SQLite format 3\x00". Anything else is treated as the legacy JSON export
	// (Uptime-Kuma removed that export in v1.23, so the database is the primary
	// migration path now).
	var mapped importexport.KumaResult
	if bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
		mapped, err = importexport.MapKumaDB(r.Context(), body)
	} else {
		mapped, err = importexport.MapKuma(body)
	}
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	res := s.applyBundle(r, mapped.Monitors, mapped.Notifications, mapped.Tags, nil, nil)
	res.Warnings = append(res.Warnings, mapped.Warnings...)
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "config.import_kuma", "", nil, clientIP(r), nil)
	writeJSON(w, http.StatusOK, res)
}

// applyBundle creates the given specs, skipping and reporting any that fail
// (e.g. name/slug conflicts, unknown types). It resolves status-page monitor
// references by name against the monitors created in this call.
func (s *server) applyBundle(r *http.Request, monitors []importexport.MonitorSpec, notifs []importexport.NotificationSpec,
	tags []importexport.TagSpec, groups []importexport.MonitorGroupSpec, pages []importexport.StatusPageSpec) importResult {
	ctx := r.Context()
	var res importResult

	// Resolve tags by name so monitors can be linked to them below. Existing
	// tags are reused; only genuinely new names are created.
	tagIDByName := map[string]int64{}
	if existing, err := s.store.ListTags(ctx); err == nil {
		for _, t := range existing {
			tagIDByName[t.Name] = t.ID
		}
	}
	for _, t := range tags {
		if _, ok := tagIDByName[t.Name]; ok {
			continue
		}
		// The same rules handleCreateTag applies. The colour is interpolated into
		// an inline style, so an unchecked one is a stored CSS injection; it is
		// replaced rather than the tag dropped, so its monitors keep the tag.
		if err := checkLength("name", t.Name, maxTagNameRunes); t.Name == "" || err != nil {
			res.Warnings = append(res.Warnings, "tag "+t.Name+": invalid name, skipped")
			continue
		}
		color, ok := resolveTagColor(t.Color)
		if !ok {
			res.Warnings = append(res.Warnings, "tag "+t.Name+": invalid colour, default used")
			color = defaultTagColor
		}
		created, err := s.store.CreateTag(ctx, t.Name, color)
		if err != nil {
			res.Warnings = append(res.Warnings, "tag "+t.Name+": "+err.Error())
			continue
		}
		tagIDByName[created.Name] = created.ID
		res.Tags++
	}

	// Notifications are created BEFORE monitors so each monitor can be wired to
	// its channels in the same pass (see NotificationIDs below). Existing
	// channels are reused by name.
	notifIDByName := map[string]int64{}
	if existing, err := s.store.ListNotifications(ctx); err == nil {
		for _, n := range existing {
			notifIDByName[n.Name] = n.ID
		}
	}
	for _, n := range notifs {
		if _, ok := notifIDByName[n.Name]; ok {
			continue
		}
		active := n.Active
		req := notificationRequest{
			Name:      n.Name,
			Type:      n.Type,
			Config:    n.Config,
			IsDefault: n.IsDefault,
			Active:    &active,
		}
		// Route imports through the same validating write path a normal create
		// uses, so unknown types and invalid configs are rejected rather than
		// stored blindly.
		if err := s.validateNotification(req); err != nil {
			res.Warnings = append(res.Warnings, "notification "+n.Name+": "+err.Error())
			continue
		}
		// Fail closed: a config that could not be sealed is never persisted.
		// notificationFromRequest reports the failure directly now; the belt-
		// and-braces check on the returned envelope stays, because this loop
		// writes secrets in bulk and a silent plaintext row here would be found
		// by nobody.
		rec, ferr := s.notificationFromRequest(req)
		if ferr != nil {
			res.Warnings = append(res.Warnings, "notification "+n.Name+": encryption failed, skipped")
			continue
		}
		if s.box != nil && !configEncrypted(rec.Config) {
			res.Warnings = append(res.Warnings, "notification "+n.Name+": encryption failed, skipped")
			continue
		}
		created, err := s.store.CreateNotification(ctx, rec)
		if err != nil {
			res.Warnings = append(res.Warnings, "notification "+n.Name+": "+err.Error())
			continue
		}
		notifIDByName[created.Name] = created.ID
		res.Notifications++
	}

	// Folders, reused by name like tags.
	groupIDByName := map[string]int64{}
	if existing, err := s.store.ListMonitorGroups(ctx); err == nil {
		for _, g := range existing {
			groupIDByName[g.Name] = g.ID
		}
	}
	for _, g := range groups {
		if _, ok := groupIDByName[g.Name]; ok {
			continue
		}
		if err := checkLength("name", g.Name, maxNameRunes); g.Name == "" || err != nil {
			res.Warnings = append(res.Warnings, "monitor group "+g.Name+": invalid name, skipped")
			continue
		}
		created, err := s.store.CreateMonitorGroup(ctx, g.Name, g.SortOrder)
		if err != nil {
			res.Warnings = append(res.Warnings, "monitor group "+g.Name+": "+err.Error())
			continue
		}
		groupIDByName[created.Name] = created.ID
	}

	// Monitors are matched by name too, as this function's caller promises for
	// every entity: re-importing a bundle used to create every monitor again.
	existingMonitors, err := s.monitorIDsByName(ctx)
	if err != nil {
		res.Warnings = append(res.Warnings, "could not list existing monitors; duplicates were not checked")
	}

	principal := principalFrom(r)
	idByName := map[string]int64{}
	for _, m := range parentsFirst(monitors) {
		// The existing monitor stands in for the skipped one, so children and
		// status pages in the bundle still find it.
		if id, ok := existingMonitors[m.Name]; ok {
			idByName[m.Name] = id
			res.Warnings = append(res.Warnings, "monitor "+m.Name+": a monitor with this name already exists, skipped")
			continue
		}
		cfg := m.Config
		if len(cfg) == 0 {
			cfg = json.RawMessage(`{}`)
		}
		// A bundle from our own export carries the redaction placeholder wherever
		// a credential used to be. On a fresh import there is no stored value to
		// restore, so the field is DROPPED and the monitor is created without the
		// secret for the operator to re-enter — which is what the export promises.
		// Leaving the placeholder in would fail type validation, because a headers
		// object masked to a string is not a headers object.
		// nil stored config: nothing to carry over, so the placeholder simply
		// drops and the refusal below cannot trigger.
		cfg, _ = s.keepRedactedSecrets(m.Type, cfg, nil)
		// A URL that carried a password inline comes out of the export with the
		// placeholder in its userinfo; the password is dropped the same way and
		// said so, because the monitor will fail its login until it is re-entered.
		cfg, droppedURLs := dropMaskedURLPasswords(cfg)
		for _, f := range droppedURLs {
			res.Warnings = append(res.Warnings,
				"monitor "+m.Name+": the password in "+f+" was redacted in the export; re-enter it")
		}

		interval := m.IntervalSeconds
		if interval < 1 {
			interval = 60
		}
		retry := m.RetryInterval
		if retry < 1 {
			retry = interval
		}
		// An unresolved reference is REPORTED, not just dropped. This function's
		// contract is "skipping and reporting any that fail", and these three name
		// lookups were the exception: a tag whose creation failed, or a channel that
		// was skipped for a bad config, left its monitor quietly unlinked. The result
		// then said "12 monitors" and the operator had no way to learn that some of
		// them lost their alert routing — which is the one association here whose
		// absence is silent at runtime too, because a monitor with no channel simply
		// never pages anyone.
		var tagIDs []int64
		for _, name := range m.Tags {
			id, ok := tagIDByName[name]
			if !ok {
				res.Warnings = append(res.Warnings,
					"monitor "+m.Name+": unknown tag "+name+", left unlinked")
				continue
			}
			tagIDs = append(tagIDs, id)
		}
		var notifIDs []int64
		for _, name := range m.Notifications {
			id, ok := notifIDByName[name]
			if !ok {
				res.Warnings = append(res.Warnings,
					"monitor "+m.Name+": unknown notification channel "+name+
						", left unlinked — this monitor will not alert anyone")
				continue
			}
			notifIDs = append(notifIDs, id)
		}
		// parentsFirst has put a parent from this bundle ahead of its children,
		// so it is in idByName by now unless it was skipped.
		var parentID, groupID *int64
		if m.Parent != "" {
			if id, ok := idByName[m.Parent]; ok {
				parentID = &id
			} else {
				res.Warnings = append(res.Warnings,
					"monitor "+m.Name+": parent "+m.Parent+" was not imported, left without a parent")
			}
		}
		if m.Group != "" {
			if id, ok := groupIDByName[m.Group]; ok {
				groupID = &id
			} else {
				res.Warnings = append(res.Warnings,
					"monitor "+m.Name+": unknown monitor group "+m.Group+", left unfiled")
			}
		}
		active := m.Active
		req := monitorRequest{
			Name:            m.Name,
			Type:            m.Type,
			Config:          cfg,
			Interval:        interval,
			Timeout:         m.Timeout,
			MaxRetries:      m.MaxRetries,
			RecoveryChecks:  m.RecoveryChecks,
			RetryInterval:   retry,
			ResendInterval:  m.ResendInterval,
			UpsideDown:      m.UpsideDown,
			Active:          &active,
			Description:     m.Description,
			LinkURL:         m.LinkURL,
			ParentID:        parentID,
			GroupID:         groupID,
			TagIDs:          tagIDs,
			NotificationIDs: notifIDs,
		}
		// Route imports through the same validating write path a normal create
		// uses, exactly as the notification loop above already did. Going straight
		// to CreateMonitor skipped the type's own config validation and the
		// link_url guard, so an import could store a monitor the scheduler cannot
		// run and a link carrying a javascript: payload.
		if err := s.validateMonitor(req); err != nil {
			res.Warnings = append(res.Warnings, "monitor "+m.Name+": "+err.Error())
			continue
		}
		if err := s.validateMonitorRelations(ctx, 0, req); err != nil {
			res.Warnings = append(res.Warnings, "monitor "+m.Name+": "+err.Error())
			continue
		}
		var createdBy *int64
		if principal != nil {
			createdBy = &principal.userID
		}
		in, err := s.monitorInput(req, createdBy)
		if err != nil {
			res.Warnings = append(res.Warnings, "monitor "+m.Name+": could not secure the configuration, skipped")
			continue
		}
		// Fail closed, mirroring the notification path: monitor configs carry
		// database passwords, bearer tokens and SNMP community strings, and an
		// import must never be the one path that writes them in the clear.
		if s.box != nil && !configEncrypted(in.Config) {
			res.Warnings = append(res.Warnings, "monitor "+m.Name+": encryption failed, skipped")
			continue
		}
		created, err := s.store.CreateMonitor(ctx, in)
		if err != nil {
			res.Warnings = append(res.Warnings, "monitor "+m.Name+": "+err.Error())
			continue
		}
		idByName[m.Name] = created.ID
		res.Monitors++
		if s.engine != nil && created.Active {
			// The store round-trips the sealed config; the engine needs plaintext.
			created.Config = s.decryptMonitorConfig(created.Config)
			if uerr := s.engine.Upsert(created); uerr != nil {
				// Reported, not swallowed: the row exists but nothing is checking
				// it, and an import that says "42 monitors" while some of them are
				// unscheduled is the worst kind of quiet failure.
				res.Warnings = append(res.Warnings, "monitor "+m.Name+": created but not scheduled: "+uerr.Error())
			}
		}
	}

	for _, p := range pages {
		if err := validateImportedPage(p); err != nil {
			res.Warnings = append(res.Warnings, "status page "+p.Slug+": "+err.Error()+", skipped")
			continue
		}
		page, warnings := pageFromSpec(p, idByName)
		res.Warnings = append(res.Warnings, warnings...)
		if _, err := s.store.CreateStatusPage(ctx, page); err != nil {
			res.Warnings = append(res.Warnings, "status page "+p.Slug+": "+err.Error())
			continue
		}
		res.StatusPages++
	}
	return res
}

// validateImportedPage applies the checks handleCreateStatusPage makes. The slug
// becomes a public URL path and the group names are the layout's join key, so
// an import must not be the one way around them.
func validateImportedPage(p importexport.StatusPageSpec) error {
	if p.Title == "" {
		return validationError("title is required")
	}
	if err := validateStatusPageSlug(p.Slug); err != nil {
		return err
	}
	groups := make([]statusPageGroupInput, 0, len(p.Groups))
	for _, g := range p.Groups {
		groups = append(groups, statusPageGroupInput{Name: g.Name, SortOrder: g.SortOrder})
	}
	return validateStatusPageGroups(groups)
}

// pageFromSpec builds the status page an imported spec describes, resolving
// monitor names through idByName, and returns the warnings the operator needs.
func pageFromSpec(p importexport.StatusPageSpec, idByName map[string]int64) (StatusPage, []string) {
	var warnings []string
	// Resolved against the bundle's monitors as this call created or matched
	// them, so a monitor the monitor loop skipped takes its status-page slot
	// with it. Reported, or the
	// page comes up listing fewer services than the bundle described and the
	// result still reads as a clean import.
	resolve := func(name string) (int64, bool) {
		id, ok := idByName[name]
		if !ok {
			warnings = append(warnings,
				"status page "+p.Slug+": monitor "+name+" was not imported, omitted from the page")
		}
		return id, ok
	}
	page := StatusPage{
		Slug:          strings.TrimSpace(p.Slug),
		Title:         p.Title,
		Description:   p.Description,
		FooterText:    p.FooterText,
		Theme:         nonEmptyTheme(p.Theme),
		AccentColor:   normalizeHexColor(p.AccentColor),
		Locale:        p.Locale,
		Published:     p.Published,
		AllowIndexing: p.AllowIndexing,
	}
	// The password cannot travel — the export carries no hash, by design — and
	// creating the page published would make a protected page public, the one
	// change to it nobody would make on purpose. It lands as a draft instead.
	if p.HasPassword {
		page.Published = false
		warnings = append(warnings, "status page "+p.Slug+
			": was password-protected; imported unpublished without a password — set one before publishing it")
	}
	for _, g := range p.Groups {
		page.Groups = append(page.Groups, StatusPageGroup{Name: g.Name, SortOrder: g.SortOrder})
	}
	if len(p.Layout) > 0 {
		for _, e := range p.Layout {
			if id, ok := resolve(e.Monitor); ok {
				page.Monitors = append(page.Monitors, StatusPageMonitorEntry{
					MonitorID: id, GroupName: e.Group, SortOrder: e.SortOrder, ShowUptime: e.ShowUptime,
				})
			}
		}
		return page, warnings
	}
	// A bundle from before Layout existed: flat, every uptime shown, which is
	// what such a page looked like.
	for _, name := range p.Monitors {
		if id, ok := resolve(name); ok {
			page.MonitorIDs = append(page.MonitorIDs, id)
		}
	}
	return page, warnings
}

// monitorIDsByName maps the name of every live monitor to its id. With
// duplicate names the first one listed wins, which is all de-duplication needs.
func (s *server) monitorIDsByName(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for offset := 0; ; offset += exportPageSize {
		page, total, err := s.store.ListMonitors(ctx, MonitorFilter{}, Page{Limit: exportPageSize, Offset: offset})
		if err != nil {
			return out, err
		}
		for _, m := range page {
			if _, seen := out[m.Name]; !seen {
				out[m.Name] = m.ID
			}
		}
		if len(page) == 0 || offset+len(page) >= total {
			return out, nil
		}
	}
}

// parentsFirst orders monitor specs so that every monitor whose parent is in the
// same list comes after that parent, keeping the bundle's order otherwise.
//
// A parent is set when the child is created, and it has to exist by then. The
// export lists monitors by id, and a child created before its parent is listed
// first. Specs whose parent chain loops are appended at the end; their parent
// is then not yet created and the import reports it.
func parentsFirst(specs []importexport.MonitorSpec) []importexport.MonitorSpec {
	inBundle := make(map[string]bool, len(specs))
	for _, m := range specs {
		inBundle[m.Name] = true
	}
	placed := make(map[string]bool, len(specs))
	out := make([]importexport.MonitorSpec, 0, len(specs))
	done := make([]bool, len(specs))
	for progress := true; progress; {
		progress = false
		for i, m := range specs {
			if done[i] || (m.Parent != "" && inBundle[m.Parent] && !placed[m.Parent]) {
				continue
			}
			out = append(out, m)
			placed[m.Name] = true
			done[i] = true
			progress = true
		}
	}
	for i, m := range specs {
		if !done[i] {
			out = append(out, m)
		}
	}
	return out
}

// configEncrypted reports whether cfg is a sealed secrets envelope
// ({"$encrypted":"<...>"}). Import uses it to fail closed: a config that is not
// sealed while a box is configured means encryption failed and must not be
// persisted.
func configEncrypted(cfg json.RawMessage) bool {
	var env struct {
		Encrypted string `json:"$encrypted"`
	}
	return json.Unmarshal(cfg, &env) == nil && env.Encrypted != ""
}

func nonEmptyTheme(t string) string {
	switch t {
	case "light", "dark", "auto":
		return t
	default:
		return "auto"
	}
}
