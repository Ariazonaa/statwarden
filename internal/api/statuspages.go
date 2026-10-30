package api

import (
	"net/http"
	"strings"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

type statusPageGroupInput struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

type statusPageMonitorInput struct {
	MonitorID  int64  `json:"monitor_id"`
	GroupName  string `json:"group_name"`
	SortOrder  int    `json:"sort_order"`
	ShowUptime *bool  `json:"show_uptime"`
}

type statusPageRequest struct {
	Slug          string                   `json:"slug"`
	Title         string                   `json:"title"`
	Description   string                   `json:"description"`
	FooterText    string                   `json:"footer_text"`
	Theme         string                   `json:"theme"`
	LogoPath      string                   `json:"logo_path"` // accepted and ignored; owned by the logo endpoints
	AccentColor   string                   `json:"accent_color"`
	CustomDomain  string                   `json:"custom_domain"`
	Locale        string                   `json:"locale"`
	Password      string                   `json:"password"`       // plaintext; hashed on save
	ClearPassword bool                     `json:"clear_password"` // drop protection
	Published     *bool                    `json:"published"`
	AllowIndexing *bool                    `json:"allow_indexing"`
	MonitorIDs    []int64                  `json:"monitor_ids"`
	Groups        []statusPageGroupInput   `json:"groups"`
	Monitors      []statusPageMonitorInput `json:"monitors"`
}

// toPage builds a StatusPage from the request. Password hashing is best-effort;
// on failure the page is saved without a (new) password rather than 500-ing.
func (r statusPageRequest) toPage() StatusPage {
	published := true
	if r.Published != nil {
		published = *r.Published
	}
	indexing := true
	if r.AllowIndexing != nil {
		indexing = *r.AllowIndexing
	}
	theme := r.Theme
	switch theme {
	case "light", "dark", "auto":
	default:
		theme = "auto"
	}
	p := StatusPage{
		Slug:        strings.TrimSpace(r.Slug),
		Title:       r.Title,
		Description: r.Description,
		FooterText:  r.FooterText,
		Theme:       theme,
		// logo_path is deliberately not mapped: it is server-owned and written
		// only by POST/DELETE /status-pages/{id}/logo. Mapping it here would let
		// a PUT that omits the key clear the column and orphan the file on disk,
		// and would let a client point the public page's <img src> anywhere.
		AccentColor:   normalizeHexColor(r.AccentColor),
		CustomDomain:  strings.ToLower(strings.TrimSpace(r.CustomDomain)),
		Locale:        strings.TrimSpace(r.Locale),
		Published:     published,
		AllowIndexing: indexing,
		MonitorIDs:    r.MonitorIDs,
	}
	// SortOrder is forwarded verbatim and is authoritative for display order, so
	// callers should send a contiguous 0..n-1 sequence: replacePageLayout and
	// MemStore.applyLayout both coerce a zero order to the item's index, which
	// silently rewrites a non-contiguous 0 into something the caller did not ask
	// for. PATCH /status-pages/{id}/layout always writes a contiguous run.
	for _, g := range r.Groups {
		p.Groups = append(p.Groups, StatusPageGroup{Name: g.Name, SortOrder: g.SortOrder})
	}
	for _, mo := range r.Monitors {
		show := true
		if mo.ShowUptime != nil {
			show = *mo.ShowUptime
		}
		p.Monitors = append(p.Monitors, StatusPageMonitorEntry{
			MonitorID:  mo.MonitorID,
			GroupName:  mo.GroupName,
			SortOrder:  mo.SortOrder,
			ShowUptime: show,
		})
	}
	if r.Password != "" {
		if hash, err := auth.HashPassword(r.Password); err == nil {
			p.PasswordHash = hash
		}
	} else if r.ClearPassword {
		p.PasswordHash = passwordClearSentinel
	}
	return p
}

// validateStatusPageGroups rejects empty and duplicate group names.
//
// The name is the JOIN KEY on this wire format: a monitor is attached to a
// group by group_name, and both layout writers build a name→id map to resolve
// it. Two groups sharing a name therefore is not merely untidy — every monitor
// naming it collapses into whichever group was written last, and the others
// render empty on the public page. An empty name can never be referenced at
// all, so a group carrying one could never hold a monitor.
func validateStatusPageGroups(groups []statusPageGroupInput) error {
	seen := make(map[string]bool, len(groups))
	for _, g := range groups {
		if g.Name == "" {
			return validationError("group names must not be empty")
		}
		if seen[g.Name] {
			return validationError("duplicate group name: " + g.Name)
		}
		seen[g.Name] = true
	}
	return nil
}

// passwordClearSentinel signals an update that should drop password protection.
// An empty PasswordHash means "leave unchanged"; this value means "clear".
const passwordClearSentinel = "\x00clear"

func (s *server) handleListStatusPages(w http.ResponseWriter, r *http.Request) {
	pages, err := s.store.ListStatusPages(r.Context())
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, pages)
}

func (s *server) handleGetStatusPage(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	p, err := s.store.GetStatusPage(r.Context(), id)
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) handleCreateStatusPage(w http.ResponseWriter, r *http.Request) {
	var req statusPageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" {
		writeProblemDetail(w, http.StatusBadRequest, "slug and title are required")
		return
	}
	if err := validateStatusPageSlug(req.Slug); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateAccentColor(req.AccentColor); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateStatusPageGroups(req.Groups); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.store.CreateStatusPage(r.Context(), req.toPage())
	if storeError(w, err) {
		return
	}
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "status_page.create", "status_page", &page.ID, clientIP(r), nil)
	writeJSON(w, http.StatusCreated, page)
}

func (s *server) handleUpdateStatusPage(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req statusPageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" {
		writeProblemDetail(w, http.StatusBadRequest, "slug and title are required")
		return
	}
	if err := validateStatusPageSlug(req.Slug); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateAccentColor(req.AccentColor); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateStatusPageGroups(req.Groups); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.store.UpdateStatusPage(r.Context(), id, req.toPage())
	if storeError(w, err) {
		return
	}
	// Audited like create and delete. A status page controls what the public is
	// told about availability, so an edit — republishing, a slug change, dropping
	// the password — is exactly the kind of change the log exists for.
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "status_page.update", "status_page", &page.ID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, page)
}

func (s *server) handleDeleteStatusPage(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	// Read the page first so its logo file can be removed once the row is gone.
	page, getErr := s.store.GetStatusPage(r.Context(), id)
	if err := s.store.DeleteStatusPage(r.Context(), id); storeError(w, err) {
		return
	}
	if getErr == nil && s.logos != nil && page.LogoPath != "" {
		_ = s.logos.Remove(page.LogoPath) // best-effort, mirrors s.audit
	}
	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "status_page.delete", "status_page", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}
