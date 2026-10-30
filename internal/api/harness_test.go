package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/maintenance"
	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
)

// fakeChecker is a registrable monitor type used to satisfy monitor-config
// validation in tests.
type fakeChecker struct{}

func (fakeChecker) Type() string                   { return "fake" }
func (fakeChecker) Validate(json.RawMessage) error { return nil }
func (fakeChecker) Check(context.Context, json.RawMessage) (domain.CheckResult, error) {
	return domain.CheckResult{Status: domain.StatusUp}, nil
}

// fakeNotifier is a registrable notification provider for tests.
type fakeNotifier struct{}

func (fakeNotifier) Type() string                   { return "fake" }
func (fakeNotifier) Validate(json.RawMessage) error { return nil }
func (fakeNotifier) Send(context.Context, json.RawMessage, domain.NotificationEvent) error {
	return nil
}

// fakeEngine records the calls the API makes so tests can assert the engine is
// driven.
type fakeEngine struct {
	mu       sync.Mutex
	upserts  []domain.Monitor
	removed  []int64
	paused   []int64
	resumed  []int64
	checked  []int64
	statuses map[int64]domain.MonitorStatus
	// scheduling backs Scheduling(); the zero value means "nothing loaded",
	// which readyz treats as healthy.
	scheduled   int
	unscheduled int

	// resumeErr and upsertErr let a test drive the paths a real engine takes
	// when a monitor is not scheduled yet, or cannot be scheduled at all.
	resumeErr error
	upsertErr error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{statuses: map[int64]domain.MonitorStatus{}}
}

func (f *fakeEngine) Scheduling() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scheduled, f.unscheduled
}

func (e *fakeEngine) Upsert(m domain.Monitor) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.upserts = append(e.upserts, m)
	return e.upsertErr
}
func (e *fakeEngine) Remove(id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed = append(e.removed, id)
}
func (e *fakeEngine) Pause(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused = append(e.paused, id)
	return nil
}
func (e *fakeEngine) Resume(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resumed = append(e.resumed, id)
	return e.resumeErr
}

// failResume makes Resume report "I do not know this monitor", which is what a
// real engine does for anything created while paused or added since startup.
// It is the branch that then loads the monitor and upserts it.
func (e *fakeEngine) failResume(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resumeErr = err
}

// failUpsert makes scheduling itself fail, as an unusable config would.
func (e *fakeEngine) failUpsert(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.upsertErr = err
}

// lastUpsert returns the monitor most recently handed to the engine.
func (e *fakeEngine) lastUpsert(t *testing.T) domain.Monitor {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.upserts) == 0 {
		t.Fatal("the engine was never asked to schedule anything")
	}
	return e.upserts[len(e.upserts)-1]
}
func (e *fakeEngine) CheckNow(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checked = append(e.checked, id)
	return nil
}
func (e *fakeEngine) StatusOf(id int64) (domain.MonitorStatus, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.statuses[id]
	return st, ok
}

func (e *fakeEngine) upsertCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.upserts)
}

// harness bundles a handler with its store and fake engine and helpers for
// authenticated requests.
type harness struct {
	t       *testing.T
	handler http.Handler
	srv     *server
	// monReg is the server's live monitor registry. Tests that need a type the
	// default harness deliberately does not carry — secretChecker, which has a
	// credential and a target — register it themselves rather than everywhere,
	// because the set of registered types is itself asserted on elsewhere.
	monReg *monreg.Registry
	store  *MemStore
	engine *fakeEngine
	maint  *maintenance.Provider
	// mails records every message the server sent, so a test can assert that a
	// mail went out (and read the link out of it) without an SMTP server.
	mails *mailCapture
}

// newMaintenanceProvider builds the shared evaluator the tests exercise. It is
// the real provider over the real loader, so a test that creates a window
// through the API goes through the same invalidation path production does.
func newMaintenanceProvider(store Store, log *slog.Logger) *maintenance.Provider {
	return maintenance.NewProvider(maintenance.ProviderOptions{
		Loader: NewMaintenanceLoader(store, log),
		Logger: log,
	})
}

// newHarness builds the default harness: no SecretsBox, i.e. what DEV_MODE
// runs, so secrets are stored as the plaintext a test can inspect directly.
func newHarness(t *testing.T) *harness {
	return newHarnessWithBox(t, nil)
}

// newHarnessWithBox is newHarness with an at-rest sealer wired in, for the tests
// that assert what a column actually holds. A nil box is DEV_MODE.
func newHarnessWithBox(t *testing.T, box SecretsBox) *harness {
	t.Helper()
	return newHarnessTweaked(t, func(o *Options) { o.SecretsBox = box })
}

// newHarnessTweaked is the harness with the assembled Options handed to tweak
// before the server is built, for a test that needs a subsystem the default
// harness leaves nil — the reporter, for instance, whose absence makes the report
// endpoints answer 503 and quietly drop out of any sweep over them.
func newHarnessTweaked(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	maint := newMaintenanceProvider(store, log)

	mails := &mailCapture{}

	opts := Options{
		Logger:          log,
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Maintenance:     maint,
		Mailer:          mails,
		// A base URL is needed for the password-reset link.
		PublicBaseURL: "https://statwarden.example",
	}
	if tweak != nil {
		tweak(&opts)
	}
	h, srv := newWithServer(opts)
	return &harness{t: t, handler: h, srv: srv, monReg: mr, store: store, engine: eng, maint: maint, mails: mails}
}

// request performs a request and returns the recorder.
func (h *harness) request(method, target string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.requestWith(method, target, body, nil)
}

// session bundles the cookies + CSRF token for an authenticated user.
type session struct {
	cookies []*http.Cookie
	csrf    string
	userID  int64
}

func (h *harness) requestWith(method, target string, body any, sess *session) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sess != nil {
		for _, c := range sess.cookies {
			req.AddCookie(c)
		}
		if sess.csrf != "" {
			req.Header.Set(csrfHeader, sess.csrf)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// requestFrom sends an unauthenticated request from a specific peer address,
// for the rate-limit and lockout buckets that key on the client IP.
func (h *harness) requestFrom(method, target string, body any, peer string) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// requestRaw sends a body verbatim, without marshalling it. Tests that are
// about how the DECODER behaves need to put bytes on the wire that no Go value
// would ever produce.
func (h *harness) requestRaw(method, target, body string, sess *session) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sess != nil {
		for _, c := range sess.cookies {
			req.AddCookie(c)
		}
		if sess.csrf != "" {
			req.Header.Set(csrfHeader, sess.csrf)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// requestKey performs a request authenticated with an API-key bearer token.
func (h *harness) requestKey(method, target, token string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// setupAdmin runs first-run setup, creating the admin and returning its
// session (cookies + CSRF token).
func (h *harness) setupAdmin() *session {
	h.t.Helper()
	rec := h.request(http.MethodPost, "/api/v1/setup", setupRequest{
		Email: "admin@example.com", Password: "supersecret", DisplayName: "Admin",
	})
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("setup = %d, body %s", rec.Code, rec.Body.String())
	}
	return sessionFromRecorder(h.t, rec)
}

// login authenticates and returns a session.
func (h *harness) login(email, password, totp string) (*session, int) {
	h.t.Helper()
	rec := h.request(http.MethodPost, "/api/v1/auth/login", loginRequest{
		Email: email, Password: password, TOTPCode: totp,
	})
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	return sessionFromRecorder(h.t, rec), rec.Code
}

func sessionFromRecorder(t *testing.T, rec *httptest.ResponseRecorder) *session {
	t.Helper()
	s := &session{cookies: rec.Result().Cookies()}
	for _, c := range s.cookies {
		if c.Name == csrfCookie {
			s.csrf = c.Value
		}
	}
	// Pull the csrf_token / user id from the body when present.
	var body struct {
		CSRF string `json:"csrf_token"`
		ID   int64  `json:"id"`
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.CSRF != "" {
		s.csrf = body.CSRF
	}
	s.userID = body.ID
	if s.userID == 0 {
		s.userID = body.User.ID
	}
	return s
}

// createSecondUser makes a non-admin user with the given role via the admin
// session and returns its login session.
func (h *harness) createUser(admin *session, email, role string) *session {
	h.t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/users", createUserRequest{
		Email: email, Password: "supersecret", Role: role,
	}, admin)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create user = %d, body %s", rec.Code, rec.Body.String())
	}
	sess, code := h.login(email, "supersecret", "")
	if code != http.StatusOK {
		h.t.Fatalf("login new user = %d", code)
	}
	return sess
}

// conditionalGet issues a GET with an If-None-Match header.
func (h *harness) conditionalGet(target, etag string, sess *session) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for _, c := range sess.cookies {
		req.AddCookie(c)
	}
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// mustJSON decodes a recorder body into v.
func mustJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}
