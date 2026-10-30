package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPA serves a built single-page app from dist: existing files are served
// directly (hashed assets under /_app/immutable/ get long-lived cache
// headers), every other path falls back to index.html for client-side
// routing. When dist contains no index.html (frontend not built into this
// binary) every request answers 503.
func SPA(dist fs.FS) http.Handler {
	_, err := fs.Stat(dist, "index.html")
	built := err == nil
	fileServer := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			http.Error(w, "frontend not built: run `make frontend` and rebuild the binary",
				http.StatusServiceUnavailable)
			return
		}

		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "." {
			if st, err := fs.Stat(dist, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(r.URL.Path, "/_app/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		serveIndex(w, r, dist)
	})
}

// serveIndex writes index.html for SPA fallback routes (never cached, so
// deploys take effect immediately).
func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	f, err := dist.Open("index.html")
	if err != nil {
		http.Error(w, "index.html unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close() //nolint:errcheck // read-only file

	w.Header().Set("Cache-Control", "no-cache")
	if rs, ok := f.(io.ReadSeeker); ok {
		st, err := f.Stat()
		if err != nil {
			http.Error(w, "index.html unavailable", http.StatusInternalServerError)
			return
		}
		http.ServeContent(w, r, "index.html", st.ModTime(), rs)
		return
	}
	// Fallback for fs implementations without seeking (e.g. fstest.MapFS).
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
}
