package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
)

// avatarHarness bundles the API handler with an avatar store writing into its
// own temp directory, so the tests can assert on the files that land on disk.
type avatarHarness struct {
	*harness
	dir string
}

func newAvatarHarness(t *testing.T) *avatarHarness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	dir := t.TempDir()
	avatars, err := NewAvatarStore(dir)
	if err != nil {
		t.Fatalf("new avatar store: %v", err)
	}
	h := New(Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Avatars:         avatars,
		PublicBaseURL:   "https://statwarden.test",
	})
	return &avatarHarness{harness: &harness{t: t, handler: h, store: store, engine: eng}, dir: dir}
}

func (h *avatarHarness) files() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatalf("read avatars dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (h *avatarHarness) upload(sess *session, data []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.requestMultipart("/api/v1/me/avatar", sess, "avatar", "avatar.png", data)
}

// avatarPathOf decodes the user out of an upload response.
func avatarPathOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var u User
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode user %q: %v", rec.Body.String(), err)
	}
	return u.AvatarPath
}

// TestAvatarUploadRoundTrip covers the happy path and the serving headers.
func TestAvatarUploadRoundTrip(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()

	rec := h.upload(admin, pngBytes(t, 64, 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, body %s", rec.Code, rec.Body.String())
	}
	avatarPath := avatarPathOf(t, rec)
	if !strings.HasPrefix(avatarPath, avatarURLPrefix) {
		t.Fatalf("avatar path = %q, want the %q prefix — an avatar must not be issued under the "+
			"logo URL space", avatarPath, avatarURLPrefix)
	}
	if names := h.files(); len(names) != 1 {
		t.Fatalf("avatars dir = %v, want exactly one file", names)
	}
	// GET /me reflects it, so the read path is wired too.
	me := h.requestWith(http.MethodGet, "/api/v1/me", nil, admin)
	if got := avatarPathOf(t, me); got != avatarPath {
		t.Errorf("GET /me avatar = %q, want %q", got, avatarPath)
	}

	// The serve route is anonymous and carries the full hardened header set.
	served := h.request(http.MethodGet, avatarPath, nil)
	if served.Code != http.StatusOK {
		t.Fatalf("serve = %d, body %s", served.Code, served.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "image/png",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    "inline",
		// frame-ancestors must be restated here: this Set replaces the global
		// security header, so dropping it would make uploaded images framable.
		"Content-Security-Policy": "default-src 'none'; sandbox; frame-ancestors 'none'",
	} {
		if got := served.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if v := served.Header().Get("Cache-Control"); !strings.Contains(v, "immutable") {
		t.Errorf("cache-control = %q, want immutable", v)
	}
}

// TestAvatarUploadTypeGate is the S4 guarantee: acceptance is decided by
// sniffing the bytes, never by what the client declares.
func TestAvatarUploadTypeGate(t *testing.T) {
	t.Run("lying part content-type is rejected", func(t *testing.T) {
		h := newAvatarHarness(t)
		admin := h.setupAdmin()
		// Declares image/png; the bytes are plain text.
		rec := h.requestMultipartTyped("/api/v1/me/avatar", admin, "avatar", "avatar.png",
			"image/png", []byte("this is definitely not a png"))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("upload = %d, want 415 — the declared part Content-Type must never decide "+
				"acceptance, body %s", rec.Code, rec.Body.String())
		}
		if names := h.files(); len(names) != 0 {
			t.Errorf("avatars dir = %v, want empty", names)
		}
	})

	t.Run("svg is rejected with a specific message", func(t *testing.T) {
		h := newAvatarHarness(t)
		admin := h.setupAdmin()
		svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
		rec := h.upload(admin, svg)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("svg upload = %d, want 415, body %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "SVG") {
			t.Errorf("detail does not mention SVG, so the user is not told why: %s", rec.Body.String())
		}
		if names := h.files(); len(names) != 0 {
			t.Errorf("avatars dir = %v, want empty", names)
		}
	})

	t.Run("oversize is rejected", func(t *testing.T) {
		h := newAvatarHarness(t)
		admin := h.setupAdmin()
		// Random pixels, so the PNG does not compress away and actually exceeds
		// the byte cap. A blank image of the same dimensions would be a few KiB
		// and would test nothing.
		rec := h.upload(admin, noisyPNGBytes(t, 900, 900))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversize upload = %d, want 413, body %s", rec.Code, rec.Body.String())
		}
		if names := h.files(); len(names) != 0 {
			t.Errorf("avatars dir = %v, want empty", names)
		}
	})

	t.Run("tiny image is rejected", func(t *testing.T) {
		h := newAvatarHarness(t)
		admin := h.setupAdmin()
		rec := h.upload(admin, pngBytes(t, 2, 2))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("2px upload = %d, want 400, body %s", rec.Code, rec.Body.String())
		}
		if names := h.files(); len(names) != 0 {
			t.Errorf("avatars dir = %v, want empty", names)
		}
	})
}

// TestAvatarReplaceRemovesPrevious pins that replacing an avatar does not leave
// the old file orphaned on disk.
func TestAvatarReplaceRemovesPrevious(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()

	first := avatarPathOf(t, h.upload(admin, pngBytes(t, 64, 64)))
	second := avatarPathOf(t, h.upload(admin, pngBytes(t, 48, 48)))
	if first == second {
		t.Fatal("the replacement reused the first path")
	}
	names := h.files()
	if len(names) != 1 {
		t.Fatalf("avatars dir = %v, want exactly one file — the replaced avatar must be deleted", names)
	}
	if names[0] != path.Base(second) {
		t.Errorf("survivor = %q, want the referenced avatar %q", names[0], path.Base(second))
	}
}

// TestAvatarDeleteIsIdempotent covers both the populated and the empty case.
func TestAvatarDeleteIsIdempotent(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()
	h.upload(admin, pngBytes(t, 64, 64))

	if rec := h.requestWith(http.MethodDelete, "/api/v1/me/avatar", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body %s", rec.Code, rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Errorf("avatars dir = %v, want empty after delete", names)
	}
	if got := avatarPathOf(t, h.requestWith(http.MethodGet, "/api/v1/me", nil, admin)); got != "" {
		t.Errorf("avatar path after delete = %q, want empty", got)
	}
	if rec := h.requestWith(http.MethodDelete, "/api/v1/me/avatar", nil, admin); rec.Code != http.StatusNoContent {
		t.Errorf("second delete = %d, want 204 (idempotent)", rec.Code)
	}
}

// TestAvatarViewerCanUpload is the regression guard for the one structural
// difference from the status-page logo endpoints: those sit behind
// requireRole(roleWriter), and putting the avatar routes there too would leave
// a viewer with no way to set their own picture.
func TestAvatarViewerCanUpload(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()
	viewer := h.createUser(admin, "viewer@example.com", "viewer")

	rec := h.upload(viewer, pngBytes(t, 64, 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer upload = %d, want 200 — a viewer owns their own profile and must not "+
			"need the writer role to set an avatar, body %s", rec.Code, rec.Body.String())
	}
}

// TestAvatarUploadIsScopedToCaller pins that the endpoint has no way to address
// another user: two users uploading must end up with two distinct avatars, and
// neither may touch the other's row.
func TestAvatarUploadIsScopedToCaller(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()
	other := h.createUser(admin, "other@example.com", "user")

	adminPath := avatarPathOf(t, h.upload(admin, pngBytes(t, 64, 64)))
	otherPath := avatarPathOf(t, h.upload(other, pngBytes(t, 48, 48)))
	if adminPath == otherPath {
		t.Fatal("two users share one avatar path")
	}
	// The admin's avatar is untouched by the other user's upload.
	if got := avatarPathOf(t, h.requestWith(http.MethodGet, "/api/v1/me", nil, admin)); got != adminPath {
		t.Errorf("admin avatar = %q after another user uploaded, want %q — the endpoint must be "+
			"scoped to the caller", got, adminPath)
	}
	if names := h.files(); len(names) != 2 {
		t.Errorf("avatars dir = %v, want two files", names)
	}
}

// TestAvatarRequiresCSRF pins that the upload is a mutation behind the
// double-submit check.
func TestAvatarRequiresCSRF(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()
	stripped := &session{cookies: admin.cookies} // cookies but no CSRF header

	if rec := h.upload(stripped, pngBytes(t, 64, 64)); rec.Code != http.StatusForbidden {
		t.Errorf("upload without CSRF = %d, want 403, body %s", rec.Code, rec.Body.String())
	}
	if names := h.files(); len(names) != 0 {
		t.Errorf("avatars dir = %v, want empty", names)
	}
}

// TestAvatarServeRejectsForgedNames pins the traversal guard on the public
// serving route. The {name} param cannot span a separator, and LogoStore.Open
// matches the server-generated name shape before touching the filesystem.
func TestAvatarServeRejectsForgedNames(t *testing.T) {
	h := newAvatarHarness(t)
	admin := h.setupAdmin()
	h.upload(admin, pngBytes(t, 64, 64))

	for _, name := range []string{
		"../../etc/passwd",
		"..%2f..%2fetc%2fpasswd",
		"%2e%2e%2fsecret.png",
		"1-00000000000000000000000000000000.png", // right shape, never issued
		".upload-123456",                         // a temp file mid-write
		"avatar.png",
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.request(http.MethodGet, avatarURLPrefix+name, nil)
			if rec.Code == http.StatusOK {
				t.Errorf("serving %q returned 200; only server-issued names may resolve", name)
			}
		})
	}
}

// TestAvatarStoreIsSeparateFromLogoStore pins the directory and URL-space
// split. Sharing one directory would put both kinds of upload in the same
// ".upload-*" temp namespace and let either serving route reach the other's
// files, since the generated name shape is identical.
func TestAvatarStoreIsSeparateFromLogoStore(t *testing.T) {
	dir := t.TempDir()
	logos, err := NewLogoStore(dir)
	if err != nil {
		t.Fatalf("logo store: %v", err)
	}
	avatars, err := NewAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("avatar store: %v", err)
	}

	logoPath, err := logos.Save(1, ".png", pngBytes(t, 32, 32))
	if err != nil {
		t.Fatalf("save logo: %v", err)
	}
	avatarPath, err := avatars.Save(1, ".png", pngBytes(t, 32, 32))
	if err != nil {
		t.Fatalf("save avatar: %v", err)
	}
	if !strings.HasPrefix(logoPath, logoURLPrefix) {
		t.Errorf("logo path = %q, want the logo prefix", logoPath)
	}
	if !strings.HasPrefix(avatarPath, avatarURLPrefix) {
		t.Errorf("avatar path = %q, want the avatar prefix", avatarPath)
	}

	// Neither store will act on the other's path: Remove ignores it entirely,
	// so a cross-space cleanup cannot delete somebody else's file.
	if err := logos.Remove(avatarPath); err != nil {
		t.Errorf("logo store Remove(avatar path) = %v, want nil", err)
	}
	f, _, err := avatars.Open(path.Base(avatarPath))
	if err != nil {
		t.Fatalf("the avatar was deleted through the logo store: %v", err)
	}
	// Windows refuses to remove a file that is still open, and t.TempDir cleans
	// up after the test, so the handle has to be released here.
	_ = f.Close()
}

// noisyPNGBytes encodes a wxh PNG of random pixels, which does not compress and
// is therefore usable for testing the byte cap.
func noisyPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatalf("random pixels: %v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestAvatarPathIsSanitizedOnRead guards the read boundary. avatar_path was a
// free nullable text column from migration 0001 with no writer at all, so any
// value a real database holds today came from outside this application — and
// the frontend interpolates it straight into an <img src>.
func TestAvatarPathIsSanitizedOnRead(t *testing.T) {
	for _, stored := range []string{
		"javascript:alert(1)",
		"https://evil.example/track.png",
		"/uploads/logos/1-00000000000000000000000000000000.png", // the other URL space
		"/uploads/avatars/../../etc/passwd",
	} {
		if got := sanitizeAvatarPath(stored); got != "" {
			t.Errorf("sanitizeAvatarPath(%q) = %q, want \"\" — only paths this server issued may "+
				"reach an <img src>", stored, got)
		}
	}
	// A genuine server-issued path survives untouched.
	good := avatarURLPrefix + "1-0123456789abcdef0123456789abcdef.png"
	if got := sanitizeAvatarPath(good); got != good {
		t.Errorf("sanitizeAvatarPath(%q) = %q, want it unchanged", good, got)
	}
}

// TestAvatarUnconfiguredReports503 pins the nil-store seam.
func TestAvatarUnconfiguredReports503(t *testing.T) {
	h := newHarness(t) // no Avatars option
	admin := h.setupAdmin()

	rec := h.requestMultipart("/api/v1/me/avatar", admin, "avatar", "avatar.png", pngBytes(t, 64, 64))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("upload without an avatar store = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	if rec := h.requestWith(http.MethodDelete, "/api/v1/me/avatar", nil, admin); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("delete without an avatar store = %d, want 503", rec.Code)
	}
}
