package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/importexport"
	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// rejectingNotifier is a registrable notifier whose config validation always
// fails, so a test can assert imports run config through validation.
type rejectingNotifier struct{}

func (rejectingNotifier) Type() string                   { return "reject" }
func (rejectingNotifier) Validate(json.RawMessage) error { return errors.New("bad config") }
func (rejectingNotifier) Send(context.Context, json.RawMessage, domain.NotificationEvent) error {
	return nil
}

// secretChecker is a monitor type that declares a credential field and
// validates its config strictly, so an export/import roundtrip can be asserted
// end to end: the secret must be redacted on the way out and must not come back
// as a literal placeholder on the way in.
type secretChecker struct{}

func (secretChecker) Type() string      { return "secretty" }
func (secretChecker) Secrets() []string { return []string{"token"} }
func (secretChecker) Check(context.Context, json.RawMessage) (domain.CheckResult, error) {
	return domain.CheckResult{Status: domain.StatusUp}, nil
}
func (secretChecker) Validate(cfg json.RawMessage) error {
	var c struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	dec := json.NewDecoder(strings.NewReader(string(cfg)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return err
	}
	if c.URL == "" {
		return errors.New("url is required")
	}
	return nil
}

// failBox is a SecretsBox whose EncryptConfig always errors, to exercise the
// fail-closed path where a secret must never be persisted in plaintext.
type failBox struct{}

func (failBox) EncryptConfig(json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("encrypt failed")
}
func (failBox) DecryptConfig(cfg json.RawMessage) (json.RawMessage, error) { return cfg, nil }

// newImportHarness builds a harness wired with the given SecretsBox (may be
// nil) and a registry carrying the "fake" and "reject" notifier types.
func newImportHarness(t *testing.T, box SecretsBox) *harness {
	t.Helper()
	store := NewMemStore()
	eng := newFakeEngine()
	mr := monreg.New()
	mr.Register(fakeChecker{})
	mr.Register(secretChecker{})
	nr := notifyreg.New()
	nr.Register(fakeNotifier{})
	nr.Register(rejectingNotifier{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	maint := newMaintenanceProvider(store, log)
	mails := &mailCapture{}
	h := New(Options{
		Logger:          log,
		Store:           store,
		Engine:          eng,
		MonitorRegistry: mr,
		NotifyRegistry:  nr,
		Maintenance:     maint,
		Mailer:          mails,
		SecretsBox:      box,
		PublicBaseURL:   "https://statwarden.example",
	})
	return &harness{t: t, handler: h, store: store, engine: eng, maint: maint, mails: mails}
}

func importNotifications(specs ...importexport.NotificationSpec) importexport.Bundle {
	return importexport.Bundle{Version: importexport.BundleVersion, Notifications: specs}
}

// TestImportEncryptsNotificationSecrets proves an imported notification config
// is sealed at rest and never lands in plaintext.
func TestImportEncryptsNotificationSecrets(t *testing.T) {
	box, err := secrets.New("test-key")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newImportHarness(t, box)
	admin := h.setupAdmin()

	bundle := importNotifications(importexport.NotificationSpec{
		Name:   "ops",
		Type:   "fake",
		Config: json.RawMessage(`{"token":"s3cr3t"}`),
		Active: true,
	})
	rec := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Notifications != 1 {
		t.Fatalf("imported notifications = %d, want 1 (warnings %v)", res.Notifications, res.Warnings)
	}

	notifs, err := h.store.ListNotifications(context.Background())
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notifs) != 1 {
		t.Fatalf("stored notifications = %d, want 1", len(notifs))
	}
	stored := string(notifs[0].Config)
	if !configEncrypted(notifs[0].Config) {
		t.Fatalf("stored config is not a sealed envelope: %s", stored)
	}
	if strings.Contains(stored, "s3cr3t") {
		t.Fatalf("secret persisted in plaintext: %s", stored)
	}
}

// TestImportFailsClosedOnEncryptionError proves that when sealing fails the
// notification is skipped rather than written in plaintext.
func TestImportFailsClosedOnEncryptionError(t *testing.T) {
	h := newImportHarness(t, failBox{})
	admin := h.setupAdmin()

	bundle := importNotifications(importexport.NotificationSpec{
		Name:   "ops",
		Type:   "fake",
		Config: json.RawMessage(`{"token":"s3cr3t"}`),
		Active: true,
	})
	rec := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Notifications != 0 {
		t.Fatalf("imported notifications = %d, want 0 on encryption failure", res.Notifications)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning about the failed encryption")
	}

	notifs, err := h.store.ListNotifications(context.Background())
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notifs) != 0 {
		t.Fatalf("notification persisted despite encryption failure: %+v", notifs)
	}
}

// TestImportValidatesNotificationConfig proves imports go through the same
// config validation as the create endpoint, so invalid records are rejected.
func TestImportValidatesNotificationConfig(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()

	bundle := importNotifications(
		importexport.NotificationSpec{Name: "bad", Type: "reject", Config: json.RawMessage(`{"x":1}`), Active: true},
		importexport.NotificationSpec{Name: "nope", Type: "does-not-exist", Config: json.RawMessage(`{}`), Active: true},
	)
	rec := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Notifications != 0 {
		t.Fatalf("imported notifications = %d, want 0 (invalid config + unknown type)", res.Notifications)
	}
	if len(res.Warnings) != 2 {
		t.Fatalf("warnings = %v, want one per rejected notification", res.Warnings)
	}

	notifs, err := h.store.ListNotifications(context.Background())
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(notifs) != 0 {
		t.Fatalf("invalid notifications persisted: %+v", notifs)
	}
}

// --- monitor export/import roundtrip ------------------------------------------

// TestMonitorRoundtripRedactsAndReimports is the trip the bundle exists for, and
// it used to be broken at both ends at once: the export emitted the raw sealed
// envelope (unusable anywhere, and re-sealed into gibberish on the way back in)
// while the import wrote whatever it was handed straight to the store, with no
// type validation and no encryption.
func TestMonitorRoundtripRedactsAndReimports(t *testing.T) {
	box, err := secrets.New("roundtrip-key")
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	h := newImportHarness(t, box)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "api", Type: "secretty", Interval: 30,
		Config: json.RawMessage(`{"url":"https://api.example","token":"super-secret"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = h.requestWith(http.MethodGet, "/api/v1/export", nil, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d, body %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "super-secret") {
		t.Fatal("the export carries the credential in the clear")
	}
	if strings.Contains(raw, "$encrypted") {
		t.Fatal("the export carries a sealed envelope; it cannot be imported anywhere")
	}
	if !strings.Contains(raw, monitorSecretPlaceholder) {
		t.Fatalf("the secret field vanished instead of being marked redacted: %s", raw)
	}

	var bundle importexport.Bundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}

	// Import into a FRESH instance, which is what an export is for.
	h2 := newImportHarness(t, box)
	admin2 := h2.setupAdmin()
	rec = h2.requestWith(http.MethodPost, "/api/v1/import", bundle, admin2)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 1 {
		t.Fatalf("imported monitors = %d, want 1; warnings %v", res.Monitors, res.Warnings)
	}

	stored, _, err := h2.store.ListMonitors(context.Background(), MonitorFilter{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored monitors = %d, want 1", len(stored))
	}
	if !configEncrypted(stored[0].Config) {
		t.Fatalf("the imported monitor was stored unencrypted: %s", stored[0].Config)
	}
	plain, err := box.DecryptConfig(stored[0].Config)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if strings.Contains(string(plain), monitorSecretPlaceholder) {
		t.Fatalf("the redaction placeholder was persisted as a real value: %s", plain)
	}
	if !strings.Contains(string(plain), "https://api.example") {
		t.Fatalf("the non-secret config did not survive the roundtrip: %s", plain)
	}
}

// An imported monitor whose type config does not validate must be reported and
// skipped, not stored for the scheduler to choke on at the next restart.
func TestImportRejectsInvalidMonitorConfig(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/import", importexport.Bundle{
		Version: importexport.BundleVersion,
		Monitors: []importexport.MonitorSpec{{
			Name: "broken", Type: "secretty", IntervalSeconds: 30,
			Config: json.RawMessage(`{"nope":true}`),
		}},
	}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 0 {
		t.Fatalf("imported %d monitors, want 0", res.Monitors)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("an invalid monitor was skipped without a warning")
	}
	stored, _, _ := h.store.ListMonitors(context.Background(), MonitorFilter{}, Page{Limit: 10})
	if len(stored) != 0 {
		t.Fatalf("an invalid monitor was stored anyway: %+v", stored)
	}
}

// The link_url guard is a stored-XSS defence, and going straight to the store
// skipped it on this path entirely.
func TestImportRejectsHostileLinkURL(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/import", importexport.Bundle{
		Version: importexport.BundleVersion,
		Monitors: []importexport.MonitorSpec{{
			Name: "xss", Type: "fake", IntervalSeconds: 30,
			LinkURL: "javascript:alert(1)",
		}},
	}, admin)
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 0 {
		t.Fatalf("a javascript: link_url was imported; warnings %v", res.Warnings)
	}
}

// Fail closed, exactly as the notification path already did: with a box
// configured, a monitor whose config could not be sealed must never be written.
func TestImportMonitorFailsClosedWhenEncryptionFails(t *testing.T) {
	h := newImportHarness(t, failBox{})
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPost, "/api/v1/import", importexport.Bundle{
		Version: importexport.BundleVersion,
		Monitors: []importexport.MonitorSpec{{
			Name: "api", Type: "fake", IntervalSeconds: 30,
			Config: json.RawMessage(`{"token":"super-secret"}`),
		}},
	}, admin)
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 0 {
		t.Fatalf("a monitor was imported despite the encryption failure")
	}
	stored, _, _ := h.store.ListMonitors(context.Background(), MonitorFilter{}, Page{Limit: 10})
	if len(stored) != 0 {
		t.Fatalf("a plaintext secret was persisted: %+v", stored)
	}
}

// The export pages through every monitor. It used to take a single read capped
// at 10 000 and say nothing when it truncated, so a large instance exported a
// partial configuration that looked complete.
func TestExportReturnsEveryMonitor(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()
	const want = 7
	for i := 0; i < want; i++ {
		rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
			Name: "m" + itoa(i), Type: "fake", Interval: 30,
		}, admin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d = %d, body %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := h.requestWith(http.MethodGet, "/api/v1/export", nil, admin)
	var bundle importexport.Bundle
	mustJSON(t, rec, &bundle)
	if len(bundle.Monitors) != want {
		t.Fatalf("exported %d monitors, want %d", len(bundle.Monitors), want)
	}
}

// A bundle from a newer Statwarden is refused instead of imported field-by-field.
//
// Version is stamped on every export precisely so this check can exist, and
// nothing read it — the field was written and never compared anywhere. The reader
// is deliberately lenient about unknown JSON fields so a bundle survives additive
// changes, which is exactly what makes the unchecked path dangerous: a spec whose
// MEANING changed rather than its name would import as plausible nonsense.
func TestImportRefusesANewerBundleVersion(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()

	bundle := importexport.Bundle{
		Version: importexport.BundleVersion + 1,
		Monitors: []importexport.MonitorSpec{{
			Name: "from-the-future", Type: "fake", IntervalSeconds: 60,
			Config: json.RawMessage(`{}`), Active: true,
		}},
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import of a newer bundle = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "newer than this server understands") {
		t.Fatalf("the refusal does not explain itself: %s", rec.Body.String())
	}
	// Nothing may have been applied before the refusal.
	mons, _, err := h.store.ListMonitors(context.Background(), MonitorFilter{}, Page{Limit: 10})
	if err != nil {
		t.Fatalf("list monitors: %v", err)
	}
	if len(mons) != 0 {
		t.Fatalf("stored %d monitors from a refused bundle", len(mons))
	}

	// The current version, and an absent one (a hand-written bundle), are accepted.
	for _, v := range []int{importexport.BundleVersion, 0} {
		bundle.Version = v
		bundle.Monitors[0].Name = "v" + itoa64(int64(v))
		got := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
		if got.Code != http.StatusOK {
			t.Fatalf("import with version %d = %d, want 200 (body %s)", v, got.Code, got.Body.String())
		}
	}
}

// An unresolved reference is reported, not silently dropped.
//
// applyBundle's contract is "skipping and reporting any that fail", and three
// name lookups were the exception. The channel case is the one that matters: a
// monitor that loses its alert routing is silent at runtime too, because a
// monitor with no channel simply never pages anyone.
func TestImportReportsUnresolvedReferences(t *testing.T) {
	h := newImportHarness(t, nil)
	admin := h.setupAdmin()

	bundle := importexport.Bundle{
		Version: importexport.BundleVersion,
		Monitors: []importexport.MonitorSpec{{
			Name: "web", Type: "fake", IntervalSeconds: 60, Config: json.RawMessage(`{}`),
			Active: true,
			// Neither exists in the bundle or the instance.
			Tags:          []string{"ghost-tag"},
			Notifications: []string{"ghost-channel"},
		}},
		StatusPages: []importexport.StatusPageSpec{{
			Slug: "public", Title: "Public", Theme: "auto",
			Monitors: []string{"web", "never-imported"},
		}},
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/import", bundle, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, body %s", rec.Code, rec.Body.String())
	}
	var res importResult
	mustJSON(t, rec, &res)
	if res.Monitors != 1 || res.StatusPages != 1 {
		t.Fatalf("import = %+v, want the monitor and the page created", res)
	}

	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"ghost-tag", "ghost-channel", "never-imported"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning mentions %q; the reference was dropped silently.\nwarnings:\n%s",
				want, joined)
		}
	}
	if !strings.Contains(joined, "will not alert anyone") {
		t.Errorf("the lost channel link is not called out as an alerting gap:\n%s", joined)
	}
}
