// Package statwarden embeds the built SvelteKit frontend (web/dist) into the
// binary. The dist directory only contains .gitkeep in a fresh checkout; run
// `make frontend` (or `cd web && npm install && npm run build`) before
// building a release binary so the real assets are embedded.
package statwarden

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/Ariazonaa/statwarden/internal/api"
)

//go:embed all:web/dist
var embeddedDist embed.FS

// DistFS returns the embedded frontend filesystem rooted at web/dist.
func DistFS() fs.FS {
	sub, err := fs.Sub(embeddedDist, "web/dist")
	if err != nil {
		// The path is a compile-time constant matching the embed directive;
		// failure here is a build defect, not a runtime condition.
		panic(err)
	}
	return sub
}

// SPAHandler serves the embedded SPA with index.html fallback. If the
// frontend was not built into this binary it responds 503 on every path.
func SPAHandler() http.Handler {
	return api.SPA(DistFS())
}
