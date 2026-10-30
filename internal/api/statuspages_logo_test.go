package api

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
	"github.com/Ariazonaa/statwarden/internal/report"
)

// logoHarness bundles the API handler with a LogoStore writing into a temp
// directory, so tests can assert on the files that actually land on disk.
type logoHarness struct {
	*harness
	dir string
}

func newLogoHarness(t *testing.T) *logoHarness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	rep := &fakeReporter{uptime: report.UptimeResult{Up: 100, Total: 100, Uptime: 1.0}}
	dir := t.TempDir()
	h := New(Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Reporter:        rep,
		Logos:           mustLogoStore(t, dir),
		PublicBaseURL:   "https://statwarden.test",
	})
	return &logoHarness{
		harness: &harness{t: t, handler: h, store: store, engine: eng},
		dir:     dir,
	}
}

// mustLogoStore builds a LogoStore over dir or fails the test.
func mustLogoStore(t *testing.T, dir string) *LogoStore {
	t.Helper()
	ls, err := NewLogoStore(dir)
	if err != nil {
		t.Fatalf("new logo store: %v", err)
	}
	return ls
}

// files returns the names stored in the uploads directory.
func (h *logoHarness) files() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatalf("read uploads dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// pngBytes encodes an opaque w×h PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// requestMultipart posts a single file part, letting the multipart writer pick
// the part Content-Type.
func (h *harness) requestMultipart(target string, sess *session, field, filename string, data []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.requestMultipartTyped(target, sess, field, filename, "", data)
}

// requestMultipartTyped posts a single file part with an explicit declared part
// Content-Type, which the upload handler must ignore in favour of sniffing.
func (h *harness) requestMultipartTyped(target string, sess *session, field, filename, partType string, data []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="`+field+`"; filename="`+filename+`"`)
	if partType != "" {
		hdr.Set("Content-Type", partType)
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		h.t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		h.t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		h.t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if sess != nil {
		for _, c := range sess.cookies {
			req.AddCookie(c)
		}
		// Mirror requestWith: a session without a token sends no header, which is
		// what lets the CSRF test build a stripped session by hand.
		if sess.csrf != "" {
			req.Header.Set(csrfHeader, sess.csrf)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// uploadLogo posts data as a logo for the page and returns the recorder.
func (h *logoHarness) uploadLogo(sess *session, pageID int64, filename string, data []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.requestMultipart("/api/v1/status-pages/"+itoa64(pageID)+"/logo", sess, "logo", filename, data)
}

func TestStatusPageLogoUploadAndServe(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	pub := true
	p := h.createPage(admin, statusPageRequest{Slug: "brand", Title: "Brand", Published: &pub})

	rec := h.uploadLogo(admin, p.ID, "logo.png", pngBytes(t, 128, 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body %s", rec.Code, rec.Body.String())
	}
	var updated StatusPage
	mustJSON(t, rec, &updated)
	if !strings.HasPrefix(updated.LogoPath, logoURLPrefix) {
		t.Fatalf("logo path = %q, want %q prefix", updated.LogoPath, logoURLPrefix)
	}
	if name := path.Base(updated.LogoPath); !logoNameRe.MatchString(name) {
		t.Fatalf("stored name %q does not match the server-generated pattern", name)
	}
	if names := h.files(); len(names) != 1 {
		t.Fatalf("uploads dir = %v, want exactly one file", names)
	}

	// No session: the serving route sits outside the auth group by design.
	served := h.request(http.MethodGet, updated.LogoPath, nil)
	if served.Code != http.StatusOK {
		t.Fatalf("serve = %d, body %s", served.Code, served.Body.String())
	}
	if ct := served.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	if v := served.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Fatalf("x-content-type-options = %q, want nosniff", v)
	}
	if v := served.Header().Get("Content-Disposition"); v != "inline" {
		t.Fatalf("content-disposition = %q, want inline", v)
	}
	// The sandbox directive would otherwise erase the global frame-ancestors on
	// this route, so the header has to carry it explicitly.
	const wantCSP = "default-src 'none'; sandbox; frame-ancestors 'none'"
	if v := served.Header().Get("Content-Security-Policy"); v != wantCSP {
		t.Fatalf("csp = %q, want %q", v, wantCSP)
	}
	if v := served.Header().Get("Cache-Control"); !strings.Contains(v, "immutable") {
		t.Fatalf("cache-control = %q, want immutable", v)
	}
}

func TestStatusPageLogoRejectsSVG(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "svg", Title: "SVG"})

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	rec := h.uploadLogo(admin, p.ID, "logo.svg", svg)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("upload = %d, want 415, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want problem+json", ct)
	}
	// Pins the message-selection branch: DetectContentType reports text/plain for
	// these bytes, never image/svg+xml, so a check on the sniffed type alone would
	// have produced the generic wording.
	if !strings.Contains(rec.Body.String(), "SVG") {
		t.Fatalf("detail does not mention SVG: %s", rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}
}

func TestStatusPageLogoRejectsDeclaredTypeSpoof(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "spoof", Title: "Spoof"})

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	// Both lies at once: a .png file name and a forged part Content-Type.
	rec := h.requestMultipartTyped("/api/v1/status-pages/"+itoa64(p.ID)+"/logo", admin,
		"logo", "logo.png", "image/png", svg)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("upload = %d, want 415, body %s", rec.Code, rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}
}

func TestStatusPageLogoRejectsOversize(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "big", Title: "Big"})

	data := append(pngBytes(t, 64, 64), make([]byte, 3<<20)...)
	rec := h.uploadLogo(admin, p.ID, "logo.png", data)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload = %d, want 413, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want problem+json", ct)
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}
}

func TestStatusPageLogoRejectsHugeDimensions(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "dim", Title: "Dim"})

	cases := []struct {
		name string
		w, h int
	}{
		{"too large", 4000, 4000},
		{"too small", 8, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.uploadLogo(admin, p.ID, "logo.png", pngBytes(t, tc.w, tc.h))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("upload = %d, want 400, body %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("content-type = %q, want problem+json", ct)
			}
			if names := h.files(); len(names) != 0 {
				t.Fatalf("uploads dir = %v, want empty", names)
			}
		})
	}
}

func TestStatusPageLogoReplaceRemovesOldFile(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "replace", Title: "Replace"})

	var first, second StatusPage
	rec := h.uploadLogo(admin, p.ID, "a.png", pngBytes(t, 64, 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("first upload = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &first)

	rec = h.uploadLogo(admin, p.ID, "b.png", pngBytes(t, 96, 96))
	if rec.Code != http.StatusOK {
		t.Fatalf("second upload = %d, body %s", rec.Code, rec.Body.String())
	}
	mustJSON(t, rec, &second)

	if first.LogoPath == second.LogoPath {
		t.Fatalf("replacement reused the path %q", first.LogoPath)
	}
	if names := h.files(); len(names) != 1 {
		t.Fatalf("uploads dir = %v, want exactly one file", names)
	}
}

// TestSetStatusPageLogoReportsReplacedPath pins the store contract the upload
// and delete handlers rely on for cleanup: the value returned is the path the
// write displaced, not the one it installed, and a page without a logo yet
// displaces nothing.
func TestSetStatusPageLogoReportsReplacedPath(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "replaced", Title: "Replaced"})
	ctx := context.Background()

	const first, second = logoURLPrefix + "1-aaaa.png", logoURLPrefix + "1-bbbb.png"
	if _, replaced, err := h.store.SetStatusPageLogo(ctx, p.ID, first); err != nil || replaced != "" {
		t.Fatalf("first set = %q, %v, want \"\", nil", replaced, err)
	}
	if _, replaced, err := h.store.SetStatusPageLogo(ctx, p.ID, second); err != nil || replaced != first {
		t.Fatalf("second set = %q, %v, want %q, nil", replaced, err, first)
	}
	// Clearing reports the displaced path too, which is what makes the delete
	// handler able to remove a file an upload installed after its own read.
	if _, replaced, err := h.store.SetStatusPageLogo(ctx, p.ID, ""); err != nil || replaced != second {
		t.Fatalf("clear = %q, %v, want %q, nil", replaced, err, second)
	}
	if _, _, err := h.store.SetStatusPageLogo(ctx, p.ID+1000, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown page = %v, want ErrNotFound", err)
	}
}

// TestStatusPageLogoConcurrentReplaceLeavesNoOrphan drives the upload handler's
// save-then-set-then-remove sequence with the writes overlapping. Reading the
// old path before the write would hand two racing uploaders the same value, and
// the loser's file would then be referenced by nothing and deleted by nothing.
func TestStatusPageLogoConcurrentReplaceLeavesNoOrphan(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "race", Title: "Race"})
	logos := mustLogoStore(t, h.dir)
	// Encoded once up front: pngBytes fails the test, which no goroutine may do.
	data := pngBytes(t, 64, 64)

	const writers = 16
	var wg sync.WaitGroup
	saved := make(chan string, writers)
	ready := make(chan struct{}, writers)
	start := make(chan struct{})
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			newPath, err := logos.Save(p.ID, ".png", data)
			if err != nil {
				t.Errorf("save: %v", err)
				ready <- struct{}{}
				return
			}
			saved <- newPath
			// Release only once every file is on disk, so the row writes really do
			// collide instead of queueing behind each other's disk I/O — without
			// the barrier the window is too narrow for the race to show up.
			ready <- struct{}{}
			<-start
			_, replaced, err := h.store.SetStatusPageLogo(context.Background(), p.ID, newPath)
			if err != nil {
				t.Errorf("set logo: %v", err)
				return
			}
			if replaced != "" && replaced != newPath {
				if err := logos.Remove(replaced); err != nil {
					t.Errorf("remove %q: %v", replaced, err)
				}
			}
		}()
	}
	for range writers {
		<-ready
	}
	close(start)
	wg.Wait()
	close(saved)

	if len(h.files()) != 1 {
		t.Fatalf("uploads dir = %v, want exactly one file", h.files())
	}
	final, err := h.store.GetStatusPage(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	// The one survivor must be the file the page actually points at, not merely
	// some file: a stale replaced path could just as well have deleted the winner.
	if got, want := h.files()[0], path.Base(final.LogoPath); got != want {
		t.Fatalf("survivor = %q, want the referenced logo %q", got, want)
	}
	var seen int
	for range saved {
		seen++
	}
	if seen != writers {
		t.Fatalf("saved %d files, want %d", seen, writers)
	}
}

func TestStatusPageLogoDeleteClearsPathAndFile(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "del", Title: "Del"})
	if rec := h.uploadLogo(admin, p.ID, "logo.png", pngBytes(t, 64, 64)); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body %s", rec.Code, rec.Body.String())
	}

	target := "/api/v1/status-pages/" + itoa64(p.ID) + "/logo"
	rec := h.requestWith(http.MethodDelete, target, nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.requestWith(http.MethodGet, "/api/v1/status-pages/"+itoa64(p.ID), nil, admin)
	var got StatusPage
	mustJSON(t, rec, &got)
	if got.LogoPath != "" {
		t.Fatalf("logo path = %q, want empty", got.LogoPath)
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}

	// Deleting again exercises the early-return branch and stays idempotent.
	if rec := h.requestWith(http.MethodDelete, target, nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("second delete = %d, want 204", rec.Code)
	}
}

func TestStatusPageDeleteRemovesLogoFile(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "gone", Title: "Gone"})
	if rec := h.uploadLogo(admin, p.ID, "logo.png", pngBytes(t, 64, 64)); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body %s", rec.Code, rec.Body.String())
	}

	rec := h.requestWith(http.MethodDelete, "/api/v1/status-pages/"+itoa64(p.ID), nil, admin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete page = %d, body %s", rec.Code, rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}
}

func TestStatusPageLogoRequiresWriter(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "rbac", Title: "RBAC"})
	// The "user" role is a writer, so "viewer" is the role that must be denied.
	viewer := h.createUser(admin, "viewer@example.com", "viewer")

	rec := h.uploadLogo(viewer, p.ID, "logo.png", pngBytes(t, 64, 64))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer upload = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}
}

func TestStatusPageLogoRequiresCSRF(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "csrf", Title: "CSRF"})

	// Same cookies, no CSRF header: multipart must not bypass the middleware.
	noCSRF := &session{cookies: admin.cookies}
	rec := h.uploadLogo(noCSRF, p.ID, "logo.png", pngBytes(t, 64, 64))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("upload without CSRF = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Fatalf("uploads dir = %v, want empty", names)
	}
}

func TestStatusPageLogoUnconfigured(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "off", Title: "Off"})

	rec := h.requestMultipart("/api/v1/status-pages/"+itoa64(p.ID)+"/logo", admin,
		"logo", "logo.png", pngBytes(t, 64, 64))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("upload = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want problem+json", ct)
	}

	// The serving route is not mounted at all, and New() substitutes a plain
	// NotFoundHandler when no static handler is configured.
	served := h.request(http.MethodGet, logoURLPrefix+"whatever.png", nil)
	if served.Code != http.StatusNotFound {
		t.Fatalf("serve = %d, want 404", served.Code)
	}
}

func TestStatusPageLogoServeRejectsTraversal(t *testing.T) {
	h := newLogoHarness(t)
	admin := h.setupAdmin()
	p := h.createPage(admin, statusPageRequest{Slug: "trav", Title: "Trav"})
	if rec := h.uploadLogo(admin, p.ID, "logo.png", pngBytes(t, 64, 64)); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name   string
		target string
	}{
		{"encoded traversal", logoURLPrefix + "..%2f..%2fetc%2fpasswd"},
		{"foreign extension", logoURLPrefix + "notmine.txt"},
		{"forged token", logoURLPrefix + "1-zzzz.png"},
		{"leaked temp file", logoURLPrefix + ".upload-123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.request(http.MethodGet, tc.target, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404, body %s", tc.target, rec.Code, rec.Body.String())
			}
		})
	}
}
