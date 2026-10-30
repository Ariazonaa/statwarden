package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register the JPEG header decoder for image.DecodeConfig
	_ "image/png"  // register the PNG header decoder for image.DecodeConfig
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	_ "golang.org/x/image/webp" // register the WebP header decoder for image.DecodeConfig
)

// Upload limits. The byte cap is enforced twice — once on the transport by
// http.MaxBytesReader and once on the decoded part — so a lying Content-Length
// cannot get past it. The dimension caps are read from the image header only,
// which is what keeps a decompression bomb from ever being expanded.
const (
	maxLogoBytes  = 2 << 20 // 2 MiB
	maxLogoDim    = 2048    // px, per side
	minLogoDim    = 16      // px, per side
	maxLogoPixels = 4_000_000
)

// imageUploadLimits parameterises decodeUploadedImage for one kind of upload.
// subject names the thing in user-facing messages ("logo", "avatar"); field is
// the multipart form field the file must arrive under.
type imageUploadLimits struct {
	field     string
	subject   string
	maxBytes  int
	minDim    int
	maxDim    int
	maxPixels int
}

var logoUploadLimits = imageUploadLimits{
	field: "logo", subject: "logo",
	maxBytes: maxLogoBytes, minDim: minLogoDim, maxDim: maxLogoDim, maxPixels: maxLogoPixels,
}

// decodeUploadedImage reads a multipart image upload, enforces the size and
// dimension caps and decides the image type. It is shared by every upload
// endpoint so the type gate exists exactly once: acceptance is decided by the
// logoExtForSniff lookup on http.DetectContentType of the leading bytes, never
// by the declared part Content-Type and never by the client file name.
//
// It writes the problem response itself and reports ok=false; the caller stops.
// The returned ext is the extension to store under.
func (s *server) decodeUploadedImage(w http.ResponseWriter, r *http.Request, lim imageUploadLimits) (data []byte, ext string, ok bool) {
	// Slack above the cap covers the multipart envelope (part headers and
	// boundaries), which is not part of the image itself.
	r.Body = http.MaxBytesReader(w, r.Body, int64(lim.maxBytes)+(1<<16))
	if err := r.ParseMultipartForm(int64(lim.maxBytes)); err != nil { //nolint:gosec // G120: bounded by the MaxBytesReader above and by its own argument
		// Only the cap is a size violation. The same call also reports a wrong
		// Content-Type, a missing boundary, a truncated body and spill-file
		// errors, and answering all of those with "too large" is a lie that
		// sends the client looking for a smaller image.
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			writeProblemDetail(w, http.StatusRequestEntityTooLarge, lim.subject+" upload is too large")
		case errors.Is(err, http.ErrNotMultipart), errors.Is(err, http.ErrMissingBoundary):
			writeProblemDetail(w, http.StatusBadRequest, lim.subject+" upload must be a multipart/form-data request")
		default:
			s.logger.Error("image upload parse failed", "subject", lim.subject, "err", err)
			writeProblemDetail(w, http.StatusInternalServerError, "could not read the upload")
		}
		return nil, "", false
	}
	// ParseMultipartForm spills anything past its memory argument onto temp
	// files, so the form has to be released explicitly. It is safe to do that
	// here rather than in the caller: everything this function returns is copied
	// into memory below, so nothing outlives the form.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile(lim.field)
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, `a "`+lim.field+`" file field is required`)
		return nil, "", false
	}
	defer file.Close() //nolint:errcheck // best-effort close

	data, err = io.ReadAll(io.LimitReader(file, int64(lim.maxBytes)+1))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "could not read the uploaded file")
		return nil, "", false
	}
	if len(data) > lim.maxBytes {
		writeProblemDetail(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("%s must be at most %d MiB", lim.subject, lim.maxBytes>>20))
		return nil, "", false
	}

	// The allowlist lookup is the type gate; the message branch below only picks
	// a more helpful wording once acceptance has already been decided.
	sniff := http.DetectContentType(data)
	ext, allowed := logoExtForSniff[sniff]
	if !allowed {
		// http.DetectContentType never reports image/svg+xml — Go's sniff table
		// carries no SVG signature, so a bare <svg> arrives as text/plain and an
		// XML-prologue SVG as text/xml. Inspect the head to say so plainly.
		head := data
		if len(head) > 1024 {
			head = head[:1024]
		}
		if strings.HasPrefix(sniff, "text/") && bytes.Contains(bytes.ToLower(head), []byte("<svg")) {
			writeProblemDetail(w, http.StatusUnsupportedMediaType,
				"SVG "+lim.subject+"s are not accepted; upload a PNG, JPEG or WebP")
		} else {
			writeProblemDetail(w, http.StatusUnsupportedMediaType, "unsupported image type; use PNG, JPEG or WebP")
		}
		return nil, "", false
	}

	// DecodeConfig reads the header only, so an image claiming huge dimensions
	// is rejected without ever allocating its pixel buffer.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, "the file is not a readable image")
		return nil, "", false
	}
	switch {
	case cfg.Width < lim.minDim || cfg.Height < lim.minDim:
		writeProblemDetail(w, http.StatusBadRequest,
			fmt.Sprintf("%s must be at least %dx%d pixels", lim.subject, lim.minDim, lim.minDim))
		return nil, "", false
	case cfg.Width > lim.maxDim || cfg.Height > lim.maxDim:
		writeProblemDetail(w, http.StatusBadRequest,
			fmt.Sprintf("%s must be at most %dx%d pixels", lim.subject, lim.maxDim, lim.maxDim))
		return nil, "", false
	case cfg.Width*cfg.Height > lim.maxPixels:
		writeProblemDetail(w, http.StatusBadRequest, lim.subject+" has too many pixels")
		return nil, "", false
	}
	return data, ext, true
}

// handleUploadStatusPageLogo stores a new logo for a status page and returns the
// updated page. The uploaded bytes decide the image type: neither the declared
// part Content-Type nor the client file name is ever consulted.
func (s *server) handleUploadStatusPageLogo(w http.ResponseWriter, r *http.Request) {
	if s.logos == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "logo uploads are not configured")
		return
	}
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	// Resolve the page before reading the body so an unknown id costs a 404
	// rather than a 2 MiB upload.
	page, err := s.store.GetStatusPage(r.Context(), id)
	if storeError(w, err) {
		return
	}

	data, ext, ok := s.decodeUploadedImage(w, r, logoUploadLimits)
	if !ok {
		return
	}

	newPath, err := s.logos.Save(page.ID, ext, data)
	if err != nil {
		s.logger.Error("status page logo save failed", "page_id", page.ID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, "could not store the logo")
		return
	}
	updated, replaced, err := s.store.SetStatusPageLogo(r.Context(), id, newPath)
	if err != nil {
		// The row still points at the old file, so the new one is orphaned.
		_ = s.logos.Remove(newPath)
		storeError(w, err)
		return
	}
	// The path the write actually displaced, not the one read before the body
	// was consumed: with two uploads in flight the pre-read value is stale for
	// the loser, whose file would then be referenced by nothing and deleted by
	// nothing.
	if replaced != "" && replaced != newPath {
		_ = s.logos.Remove(replaced) // best-effort, mirrors s.audit
	}

	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "status_page.logo_upload", "status_page", &page.ID, clientIP(r),
		map[string]string{"path": newPath, "bytes": strconv.Itoa(len(data))})
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteStatusPageLogo clears a status page's logo and removes the file.
// It is idempotent: a page without a logo reports 204 as well.
func (s *server) handleDeleteStatusPageLogo(w http.ResponseWriter, r *http.Request) {
	if s.logos == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "logo uploads are not configured")
		return
	}
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	page, err := s.store.GetStatusPage(r.Context(), id)
	if storeError(w, err) {
		return
	}
	if page.LogoPath == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_, replaced, err := s.store.SetStatusPageLogo(r.Context(), id, "")
	if storeError(w, err) {
		return
	}
	// Same window as the upload path: an upload racing this delete may have
	// moved the row on since the read above, so remove what the clear replaced.
	_ = s.logos.Remove(replaced) // best-effort, mirrors s.audit

	pr := principalFrom(r)
	s.audit(r.Context(), &pr.userID, "status_page.logo_delete", "status_page", &page.ID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleServeLogo serves an uploaded logo to anonymous status-page visitors. The
// {name} chi param cannot span a path separator and is additionally matched
// against logoNameRe inside LogoStore.Open, so no request can escape the uploads
// directory.
func (s *server) handleServeLogo(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ct, ok := logoContentTypes[strings.ToLower(filepath.Ext(name))]
	if !ok {
		writeProblemDetail(w, http.StatusNotFound, "logo not found")
		return
	}
	f, info, err := s.logos.Open(name)
	if err != nil {
		writeProblemDetail(w, http.StatusNotFound, "logo not found")
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
	// and a replaced logo is served under a new one.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// Content-Type is already set, so ServeContent will not sniff. Content-Length
	// is deliberately left to net/http: middleware.Compress(5) is global.
	http.ServeContent(w, r, name, info.ModTime(), f)
}
