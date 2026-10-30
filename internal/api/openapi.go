package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/Ariazonaa/statwarden/internal/version"
)

// openapiSpecRaw is the hand-authored OpenAPI 3.1 description of this API,
// embedded so the single binary serves its own contract with no sidecar file.
//
// It is hand-written rather than generated: a generator would need annotations
// on every handler and would replace the drift guard with a build step nobody
// reviews. Instead openapi_contract_test.go walks the live chi router and fails
// the build when this file and the router disagree. Adding a route means adding
// it here too — the test names the offending path.
//
//go:embed openapi.json
var openapiSpecRaw []byte

// openapiDoc holds the served document: the embedded bytes with the two
// instance-dependent fields patched in. It is built once per server rather than
// per request, since re-marshalling the whole document on every hit of a public
// endpoint would be pure waste. The state lives on the server (not in a package
// variable) because servers.url is derived from that server's base URL, and two
// servers in one process — which is exactly what the test suite builds — must
// not share it.
type openapiDoc struct {
	once  sync.Once
	bytes []byte
}

// document returns the spec with info.version set to this build and
// servers[0].url set to baseURL.
func (d *openapiDoc) document(baseURL string) []byte {
	d.once.Do(func() {
		// Default to the embedded bytes, so any patch failure still serves a
		// valid — if slightly generic — document.
		d.bytes = openapiSpecRaw

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(openapiSpecRaw, &doc); err != nil {
			// The embedded file is a compile-time artefact of this package, so
			// a parse failure is a build defect rather than a runtime
			// condition. DistFS treats a bad embed the same way.
			panic("api: embedded openapi.json is not valid JSON: " + err.Error())
		}

		var info map[string]any
		if err := json.Unmarshal(doc["info"], &info); err == nil {
			info["version"] = version.Version
			if raw, err := json.Marshal(info); err == nil {
				doc["info"] = raw
			}
		}
		if base := strings.TrimRight(baseURL, "/"); base != "" {
			srv := []map[string]string{{"url": base, "description": "This instance"}}
			if raw, err := json.Marshal(srv); err == nil {
				doc["servers"] = raw
			}
		}
		if raw, err := json.Marshal(doc); err == nil {
			d.bytes = raw
		}
	})
	return d.bytes
}

// handleOpenAPISpec serves the OpenAPI description. It is public, like
// /version: the document describes route shapes only and carries no instance
// data, and being fetchable without a credential is what makes it usable from
// curl, Postman and client generators.
func (s *server) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	doc := s.openapi.document(s.baseURL)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(doc)
}
