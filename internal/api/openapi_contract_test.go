package api

// Drift guard for internal/api/openapi.json.
//
// The OpenAPI document is hand-authored. Nothing in the compiler, the linter or
// the frontend build notices when a route is added, renamed or deleted in
// router.go and the spec is not updated with it — the document simply becomes
// quietly wrong, which is worse than absent: an integrator trusts it, generates
// a client from it, and discovers the truth in production.
//
// This test closes that loop. It walks the LIVE chi tree that New() builds and
// asserts the set of (method, path) pairs is exactly the set openapi.json
// documents.
//
// Unlike registry_contract_test.go, which is deliberately one-directional, this
// check runs BOTH WAYS:
//
//   - a route the router serves but the spec omits is an undocumented endpoint;
//   - a path or operation the spec declares but the router does not serve sends
//     integrators at a URL that answers 404.
//
// Both are silent failures today and both mislead, so both fail here.
//
// Route registration in router.go is conditional on Options fields, so the
// router below is built with Store, Hub, Logos and Metrics all non-nil. With
// any of them nil roughly ninety routes vanish from the walk and this test
// would demand their deletion from the spec — which is why the tripwires below
// fail loudly on an empty walk rather than passing vacuously.
//
// Please do not delete this as an awkward test. It is the only thing tying the
// published contract to the code that serves it.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// openapiSpecPath is the authored document, relative to internal/api.
const openapiSpecPath = "openapi.json"

// stubWSHub satisfies WSHub so the /ws route registers. The route is excluded
// from the comparison (see excludedRoutes), but it must exist for the walk to
// reflect a production router.
type stubWSHub struct{}

func (stubWSHub) ServeWS(http.ResponseWriter, *http.Request, func() bool) {}

// excludedRoutes are the (method, path) pairs deliberately left out of the
// spec. Each entry is listed individually and justified: a prefix match would
// silently swallow a future route that genuinely belongs in the document.
var excludedRoutes = map[string]string{
	// The SPA catch-all. It answers any unmatched path with index.html and is
	// an HTML application, not an API operation.
	"GET /*":     "SPA catch-all",
	"POST /*":    "SPA catch-all",
	"PUT /*":     "SPA catch-all",
	"PATCH /*":   "SPA catch-all",
	"DELETE /*":  "SPA catch-all",
	"HEAD /*":    "SPA catch-all",
	"OPTIONS /*": "SPA catch-all",
	"CONNECT /*": "SPA catch-all",
	"TRACE /*":   "SPA catch-all",
	"QUERY /*":   "SPA catch-all",

	// Same SPA bundle, mounted separately only so public status pages keep a
	// permissive frame-ancestors header. Also not an API operation.
	"GET /status/*":     "SPA catch-all (framable status pages)",
	"POST /status/*":    "SPA catch-all (framable status pages)",
	"PUT /status/*":     "SPA catch-all (framable status pages)",
	"PATCH /status/*":   "SPA catch-all (framable status pages)",
	"DELETE /status/*":  "SPA catch-all (framable status pages)",
	"HEAD /status/*":    "SPA catch-all (framable status pages)",
	"OPTIONS /status/*": "SPA catch-all (framable status pages)",
	"CONNECT /status/*": "SPA catch-all (framable status pages)",
	"TRACE /status/*":   "SPA catch-all (framable status pages)",
	"QUERY /status/*":   "SPA catch-all (framable status pages)",

	// A WebSocket upgrade. OpenAPI 3.1 has no way to describe the protocol
	// switch or the message stream that follows, so documenting it as a GET
	// would describe something that does not exist.
	"GET /ws": "WebSocket upgrade, not expressible in OpenAPI",

	// /metrics is registered with r.Handle, i.e. for every method chi knows.
	// Only GET is meaningful and only GET is documented; the rest are an
	// artefact of mALL registration, not a contract.
	"POST /metrics":    "r.Handle registers all methods; only GET is meaningful",
	"PUT /metrics":     "r.Handle registers all methods; only GET is meaningful",
	"PATCH /metrics":   "r.Handle registers all methods; only GET is meaningful",
	"DELETE /metrics":  "r.Handle registers all methods; only GET is meaningful",
	"HEAD /metrics":    "r.Handle registers all methods; only GET is meaningful",
	"OPTIONS /metrics": "r.Handle registers all methods; only GET is meaningful",
	"CONNECT /metrics": "r.Handle registers all methods; only GET is meaningful",
	"TRACE /metrics":   "r.Handle registers all methods; only GET is meaningful",
	"QUERY /metrics":   "r.Handle registers all methods; only GET is meaningful",
}

// openapiMethods are the HTTP methods an OpenAPI path item may carry as
// operations. Anything else in a path item (parameters, summary, servers) is
// not an operation and is skipped.
var openapiMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func TestOpenAPIMatchesRouter(t *testing.T) {
	routed := walkRoutes(t)
	specced := specRoutes(t)

	for key := range routed {
		if _, ok := specced[key]; !ok {
			t.Errorf("undocumented endpoint: the router serves %q but %s does not describe it.\n"+
				"Add the operation to that file, or — if it genuinely is not an API operation — "+
				"add it to excludedRoutes in this test with a reason.",
				key, openapiSpecPath)
		}
	}
	for key := range specced {
		if _, ok := routed[key]; !ok {
			t.Errorf("spec documents a route that does not exist: %s describes %q, but the "+
				"router registers no such operation.\nRemove it from the spec, or fix the "+
				"path/method to match mountV1 in router.go.",
				openapiSpecPath, key)
		}
	}
}

// walkRoutes returns the (method, path) pairs the live router serves, minus the
// deliberate exclusions.
func walkRoutes(t *testing.T) map[string]struct{} {
	t.Helper()

	logos, err := NewLogoStore(t.TempDir())
	if err != nil {
		t.Fatalf("logo store: %v", err)
	}
	// Every Option below gates route registration. Options that only change
	// handler BEHAVIOUR (Dispatcher, Reporter, PushRecorder, Mailer, ...) are
	// left nil on purpose: they do not affect the shape of the tree.
	avatars, err := NewAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("avatar store: %v", err)
	}
	h := New(Options{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:   NewMemStore(),
		Engine:  newFakeEngine(),
		Hub:     stubWSHub{},
		Logos:   logos,
		Avatars: avatars,
		Metrics: http.NotFoundHandler(),
		// A provider is configured so the conditional SSO routes register and
		// are therefore compared against the spec. Constructing one performs no
		// network I/O — discovery is lazy — so this costs nothing here.
		OIDC:          []*OIDCProvider{{ID: "sso", Issuer: "https://idp.example", ClientID: "x"}},
		PublicBaseURL: "https://statwarden.example",
	})

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("New() no longer returns a chi.Routes (%T); this test cannot walk the tree. "+
			"Fix the walk rather than deleting the check.", h)
	}

	out := map[string]struct{}{}
	err = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := strings.ToUpper(method) + " " + route
		if _, skip := excludedRoutes[key]; skip {
			return nil
		}
		out[key] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("walked zero routes out of the router — either chi.Walk changed or the " +
			"Options above stopped satisfying the registration guards in New/mountV1. " +
			"Fix the walk rather than deleting the test.")
	}
	return out
}

// specRoutes returns the (method, path) pairs openapi.json declares.
func specRoutes(t *testing.T) map[string]struct{} {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(openapiSpecPath))
	if err != nil {
		t.Fatalf("read %s: %v", openapiSpecPath, err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openapiSpecPath, err)
	}

	out := map[string]struct{}{}
	for path, item := range doc.Paths {
		for field := range item {
			if !openapiMethods[strings.ToLower(field)] {
				continue // parameters, summary, description, servers, ...
			}
			out[strings.ToUpper(field)+" "+path] = struct{}{}
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed zero operations out of %s — the document is empty or its shape "+
			"changed. Fix the parser rather than deleting the test.", openapiSpecPath)
	}
	return out
}

// TestOpenAPISpecServed asserts the embedded document is served, is valid JSON,
// and carries this build's version and the configured server URL.
func TestOpenAPISpecServed(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodGet, "/api/v1/openapi.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served spec is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	if doc.Info.Version == "" {
		t.Error("info.version is empty; the build version was not patched in")
	}
	if len(doc.Paths) == 0 {
		t.Error("served spec declares no paths")
	}
}

// TestOpenAPISpecIsPublic pins the deliberate decision that the document needs
// no credential: it describes route shapes only, and codegen tools fetch it
// before any login exists.
func TestOpenAPISpecIsPublic(t *testing.T) {
	h := newHarness(t)
	if rec := h.request(http.MethodGet, "/api/v1/openapi.json", nil); rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET /api/v1/openapi.json = %d, want 200", rec.Code)
	}
}

// TestOpenAPIExclusionsAreReachable keeps the exclusion list honest: an entry
// that no longer matches a registered route is stale and would hide a genuinely
// undocumented endpoint if the path were ever reused.
func TestOpenAPIExclusionsAreReachable(t *testing.T) {
	logos, err := NewLogoStore(t.TempDir())
	if err != nil {
		t.Fatalf("logo store: %v", err)
	}
	avatars, err := NewAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("avatar store: %v", err)
	}
	h := New(Options{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:   NewMemStore(),
		Engine:  newFakeEngine(),
		Hub:     stubWSHub{},
		Logos:   logos,
		Avatars: avatars,
		Metrics: http.NotFoundHandler(),
		// A provider is configured so the conditional SSO routes register and
		// are therefore compared against the spec. Constructing one performs no
		// network I/O — discovery is lazy — so this costs nothing here.
		OIDC:          []*OIDCProvider{{ID: "sso", Issuer: "https://idp.example", ClientID: "x"}},
		PublicBaseURL: "https://statwarden.example",
	})
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("New() no longer returns a chi.Routes (%T)", h)
	}
	live := map[string]struct{}{}
	_ = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		live[strings.ToUpper(method)+" "+route] = struct{}{}
		return nil
	})

	var stale []string
	for key := range excludedRoutes {
		if _, ok := live[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("excludedRoutes lists %q, which the router no longer registers. "+
			"Drop the stale entry so a future route on that path is not silently exempted.", key)
	}
}
