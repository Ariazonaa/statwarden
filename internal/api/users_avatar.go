package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// avatarStoreUnavailableMsg is the problem-detail returned when the avatar store
// is absent. The store is only nil when NewAvatarStore failed at startup — which
// only happens when its directory could not be created or is not writable — so
// this points the operator straight at the cause instead of the vaguer "not
// configured". The frontend surfaces this detail verbatim.
const avatarStoreUnavailableMsg = "avatar uploads are unavailable: the uploads directory could not be created or is not writable by the app user"

// Avatar limits. Smaller than a status-page logo in every dimension: an avatar
// is rendered at ~32px in the top bar and never full-bleed, so a 1 MiB / 1024px
// ceiling is generous. The caps are enforced the same way logos are — the byte
// cap twice (transport and decoded part), the dimensions from the image header
// only, so a decompression bomb is never expanded.
const (
	maxAvatarBytes  = 1 << 20 // 1 MiB
	maxAvatarDim    = 1024    // px, per side
	minAvatarDim    = 16      // px, per side
	maxAvatarPixels = 1_100_000
)

var avatarUploadLimits = imageUploadLimits{
	field: "avatar", subject: "avatar",
	maxBytes: maxAvatarBytes, minDim: minAvatarDim, maxDim: maxAvatarDim, maxPixels: maxAvatarPixels,
}

// handleUploadAvatar stores a new avatar for the CALLING user and returns the
// updated user.
//
// There is deliberately no {id} path parameter and no user_id form field: the
// subject is principalFrom(r).userID and nothing else. Accepting an id here
// would make this an IDOR that lets any authenticated user overwrite anybody's
// avatar — the same reasoning that scopes session revocation to the caller's
// own sessions.
//
// It also sits outside the roleWriter group the status-page logo endpoints use.
// A viewer owns their own profile; the write touches only their own row.
func (s *server) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	if s.avatars == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, avatarStoreUnavailableMsg)
		return
	}
	p := principalFrom(r)

	data, ext, ok := s.decodeUploadedImage(w, r, avatarUploadLimits)
	if !ok {
		return
	}

	newPath, err := s.avatars.Save(p.userID, ext, data)
	if err != nil {
		s.logger.Error("avatar save failed", "user_id", p.userID, "err", err)
		// A permission error here is the runtime twin of the startup probe: the
		// store was writable when the process booted but the write is now denied
		// (a bind mount remounted read-only, ownership changed under it). Report it
		// as a 503 that names the cause rather than an opaque 500, so the operator
		// knows to fix the uploads directory instead of hunting a phantom app bug.
		if errors.Is(err, fs.ErrPermission) {
			writeProblemDetail(w, http.StatusServiceUnavailable,
				"could not store the avatar: the uploads directory is not writable by the app user")
			return
		}
		writeProblemDetail(w, http.StatusInternalServerError, "could not store the avatar")
		return
	}
	updated, replaced, err := s.store.SetUserAvatar(r.Context(), p.userID, newPath)
	if err != nil {
		// The row still points at the old file, so the new one is orphaned.
		_ = s.avatars.Remove(newPath)
		storeError(w, err)
		return
	}
	// The path the write actually displaced, not one read before the body was
	// consumed: with two uploads in flight the pre-read value is stale for the
	// loser, whose file would then be referenced by nothing and deleted by
	// nothing.
	if replaced != "" && replaced != newPath {
		_ = s.avatars.Remove(replaced) // best-effort, mirrors s.audit
	}

	s.audit(r.Context(), &p.userID, "user.avatar_upload", "user", &p.userID, clientIP(r),
		map[string]string{"path": newPath, "bytes": strconv.Itoa(len(data))})
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteAvatar clears the caller's avatar and removes the file. It is
// idempotent: a user without an avatar reports 204 as well.
func (s *server) handleDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	if s.avatars == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, avatarStoreUnavailableMsg)
		return
	}
	p := principalFrom(r)
	_, replaced, err := s.store.SetUserAvatar(r.Context(), p.userID, "")
	if storeError(w, err) {
		return
	}
	if replaced == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Same window as the upload path: an upload racing this delete may have
	// moved the row on, so remove what the clear actually replaced.
	_ = s.avatars.Remove(replaced) // best-effort, mirrors s.audit

	s.audit(r.Context(), &p.userID, "user.avatar_delete", "user", &p.userID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleServeAvatar serves an uploaded avatar. The {name} chi param cannot span
// a path separator and is additionally matched against logoNameRe inside
// LogoStore.Open, so no request can escape the uploads directory.
//
// The route is anonymous, like the logo route. The 128-bit file name is the only
// access control: anyone holding the URL can fetch the image without a session,
// and the immutable Cache-Control means a proxy may retain it after deletion.
// That is an accepted trade-off — an avatar is rendered in the same contexts a
// logo is, and gating it behind the session would break every cached <img> —
// but it is a deliberate one, not an oversight.
func (s *server) handleServeAvatar(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ct, ok := logoContentTypes[strings.ToLower(filepath.Ext(name))]
	if !ok {
		writeProblemDetail(w, http.StatusNotFound, "avatar not found")
		return
	}
	f, info, err := s.avatars.Open(name)
	if err != nil {
		writeProblemDetail(w, http.StatusNotFound, "avatar not found")
		return
	}
	defer f.Close() //nolint:errcheck // best-effort close

	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	// Defence in depth on a user-supplied byte stream. frame-ancestors is
	// repeated here because this Set replaces the global security header, and
	// dropping it would make uploaded files framable by anyone.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	// The file name carries 128 bits of entropy, so the URL is content-addressed
	// and a replaced avatar is served under a new one.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, info.ModTime(), f)
}
