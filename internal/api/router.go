// Package api assembles the chi HTTP router: base middleware, health and
// version endpoints, the authenticated /api/v1 REST surface, the WebSocket
// upgrade, and the embedded-SPA fallback.
package api

import (
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
	notifyreg "github.com/Ariazonaa/statwarden/internal/notify/registry"
)

// Cookie names and auth defaults.
const (
	sessionCookie = "statwarden_session"
	csrfCookie    = "statwarden_csrf"
	csrfHeader    = "X-CSRF-Token"

	defaultSessionTTL = 30 * 24 * time.Hour
	loginRateLimit    = 10 // attempts
	loginRateWindow   = time.Minute
	lockoutThreshold  = 5
	lockoutDuration   = 15 * time.Minute
	publicRateLimit   = 60
	publicRateWindow  = time.Minute

	// Push heartbeats are scheduled machine traffic, so they get their own
	// budget instead of sharing the browser-shaped public-page one.
	//
	// The limit that matters is PER TOKEN, because a token identifies exactly one
	// monitor and therefore one legitimate reporting cadence. Keying only on the
	// client IP — which is what this endpoint used to do, on the 60/min public
	// budget — punishes the normal deployment where many push monitors report
	// from one egress address: past 60 beats a minute the rest are rejected, the
	// watchdog sees no heartbeat and trips healthy monitors DOWN. A false outage
	// alert is the worst failure mode this system has, so the per-IP brake is
	// kept only as a coarse flood guard and set far above any real workload.
	pushTokenRateLimit = 120
	pushIPRateLimit    = 1200
	pushRateWindow     = time.Minute

	// settingSessionTTLDays is the settings row an admin may use to override
	// Options.SessionTTL at runtime. The bounds are enforced twice: when the row
	// is written and again every time it is read (see session_ttl.go).
	//
	// The floor exists to prevent a lockout. A TTL at or below zero mints
	// sessions that are already expired, which the auth middleware deletes on
	// sight, so nobody — including the admin who set it — could log back in to
	// undo it. The ceiling exists because an effectively permanent session cookie
	// on a shared machine defeats the point of having the knob at all. The
	// granularity is whole days, so the floor is a day rather than an hour.
	settingSessionTTLDays = "session_ttl_days"
	minSessionTTL         = 24 * time.Hour
	maxSessionTTL         = 365 * 24 * time.Hour

	// passwordResetTTL bounds how long a mailed reset link stays redeemable.
	// Deliberately short — three orders of magnitude tighter than a session —
	// because the token sits in a mailbox, which is a weaker store than a cookie.
	passwordResetTTL = time.Hour

	// oidcCookie carries the OIDC flow state (PKCE verifier, state, nonce, and
	// the id of the user a link-mode flow belongs to). Its path is scoped to the
	// OIDC routes so it is not attached to every request in the app.
	oidcCookie     = "statwarden_oidc"
	oidcCookiePath = "/api/v1/auth/oidc"
	oidcFlowTTL    = 5 * time.Minute
)

// Options configures the router.
type Options struct {
	// Logger receives structured request and panic logs. Required.
	Logger *slog.Logger
	// Pool is the database pool used by /readyz. May be nil (DEV_MODE
	// without a reachable database); /readyz then reports 503.
	Pool *pgxpool.Pool
	// Static serves the embedded SPA for all non-API routes.
	Static http.Handler
	// Store is the persistence surface for the REST API. When nil, the API
	// routes are not mounted (only health/version/SPA), preserving the
	// original minimal behaviour for callers that only want static serving.
	Store Store
	// Engine is driven by monitor mutations (create/edit/delete/pause). May
	// be nil, in which case mutations persist but do not live-reload.
	Engine Engine
	// Dispatcher backs the notification "test send" endpoint. May be nil.
	Dispatcher Dispatcher
	// Hub is the WebSocket hub; when nil the /ws route is not mounted.
	Hub WSHub
	// MonitorRegistry and NotifyRegistry list available types for the
	// dynamic-config endpoints; default to the process-wide registries.
	MonitorRegistry *monreg.Registry
	NotifyRegistry  *notifyreg.Registry
	// SecretsBox encrypts notification configs at rest; may be nil (configs
	// stored as plaintext, DEV_MODE only).
	SecretsBox SecretsBox
	// Reporter backs the uptime/report/badge endpoints. May be nil, in which
	// case those endpoints report 503.
	Reporter Reporter
	// PushRecorder backs the public push heartbeat endpoint. May be nil, in
	// which case /push/{token} reports 503.
	PushRecorder PushRecorder
	// AuditSink forwards every persisted audit entry to an external collector
	// (SIEM, log shipper). May be nil, which is the normal state of an instance
	// with no collector configured; audit entries are still written locally and
	// still downloadable as CSV.
	AuditSink AuditSink
	// Metrics is the Prometheus /metrics handler; nil disables the endpoint.
	Metrics http.Handler
	// MetricsToken is the scrape token configured for /metrics. When empty the
	// endpoint is closed by default (a 404) even if Metrics is non-nil, so an
	// operator who never set a token does not silently expose the metrics — the
	// leak the previous "empty token means open" behaviour created.
	MetricsToken string
	// Logos stores and serves uploaded status-page logos. When nil the upload
	// endpoints report 503 and the /uploads/logos route is not mounted.
	Logos *LogoStore
	// Avatars stores and serves uploaded user avatars. When nil the upload
	// endpoints report 503 and the /uploads/avatars route is not mounted. It
	// MUST point at a different directory than Logos: sharing one would put both
	// kinds of file in the same ".upload-*" temp namespace and let either store
	// serve the other's files, since the generated name shape is the same.
	Avatars *LogoStore
	// OIDC lists the configured single-sign-on providers. Empty (the default)
	// means SSO is off and none of the /auth/oidc routes are mounted.
	OIDC []*OIDCProvider
	// Mailer delivers status-page subscriber e-mails (verification, incident
	// notices). May be nil, in which case those mails are silently skipped (a
	// clean seam: subscribers and tokens are still recorded).
	Mailer Mailer
	// WSPublisher broadcasts public status-page updates over the WebSocket hub.
	// May be nil. *ws.Hub satisfies it.
	WSPublisher WSPublisher
	// Maintenance is the shared maintenance evaluator — the same instance the
	// engine suppresses alerts with. Maintenance mutations refresh it and the
	// public status page reads it. May be nil (no window ever in effect).
	Maintenance MaintenanceProvider
	// PublicBaseURL is used to render absolute push URLs; optional.
	PublicBaseURL string
	// AllowedOrigins are extra browser origins (beyond PublicBaseURL) accepted by
	// the WebSocket origin check, for an instance reached under more than one
	// hostname. Each is an absolute http(s) URL. Empty is the norm.
	AllowedOrigins []string
	// SecureCookies forces the Secure flag on auth cookies (set when the
	// public base URL is https).
	SecureCookies bool
	// TrustedProxies lists the CIDRs of reverse proxies whose X-Forwarded-For
	// header is believed when resolving the client IP. Empty (the default)
	// means the peer address is always used verbatim.
	TrustedProxies []netip.Prefix
	// SessionSecret keys the HMAC protecting the OIDC flow cookie. Required
	// outside DEV_MODE by config.Load; empty leaves that cookie unsigned.
	SessionSecret string
	// SessionTTL overrides the default session lifetime.
	SessionTTL time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

type server struct {
	logger         *slog.Logger
	pool           *pgxpool.Pool
	store          Store
	engine         Engine
	dispatcher     Dispatcher
	hub            WSHub
	monReg         *monreg.Registry
	notifyReg      *notifyreg.Registry
	box            SecretsBox
	reporter       Reporter
	push           PushRecorder
	auditSink      AuditSink
	metrics        http.Handler
	metricsToken   string
	logos          *LogoStore
	avatars        *LogoStore
	mailer         Mailer
	wsPub          WSPublisher
	maint          MaintenanceProvider
	baseURL        string
	allowedOrigins []string

	// oidc indexes the configured providers by their local id. Empty means SSO
	// is not configured and the routes are not mounted.
	oidc map[string]*OIDCProvider

	// sessionSecret keys the HMAC on the OIDC flow cookie. Empty in DEV_MODE,
	// where the cookie is left unsigned.
	sessionSecret string

	secureCookies  bool
	sessionTTL     time.Duration
	trustedProxies []netip.Prefix
	now            func() time.Time

	loginLimiter *rateLimiter
	// resetLimiter bounds password-reset mail PER ADDRESS, which the per-IP
	// limiter cannot: the address being mailed belongs to a third party, so
	// without this a rotating set of clients can use this endpoint to bury
	// somebody's inbox. Exceeding it silently skips issuing — the response is
	// the same 202 either way, or the limiter itself would answer the question
	// the endpoint refuses to.
	resetLimiter *rateLimiter
	// resetInflight counts reset issuance still running after the response.
	// Tests wait on it; production never reads it.
	resetInflight sync.WaitGroup
	// publicLimiter guards the browser-facing public endpoints; pushLimiter and
	// pushIPLimiter guard the push heartbeat endpoint on their own budgets (see
	// pushTokenRateLimit).
	publicLimiter *rateLimiter
	pushLimiter   *rateLimiter
	pushIPLimiter *rateLimiter
	lockout       *lockout

	// openapi lazily renders the embedded API description for /openapi.json.
	openapi openapiDoc
}

// New builds the full HTTP handler.
func New(o Options) http.Handler {
	h, _ := newWithServer(o)
	return h
}

// newWithServer is New plus the server value behind the handler. It exists for
// tests that have to wait on work the server deliberately does AFTER the
// response — password-reset issuance, which is off the request path so that a
// known address and an unknown one take the same time.
func newWithServer(o Options) (http.Handler, *server) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Static == nil {
		o.Static = http.NotFoundHandler()
	}
	if o.MonitorRegistry == nil {
		o.MonitorRegistry = monreg.Default
	}
	if o.NotifyRegistry == nil {
		o.NotifyRegistry = notifyreg.Default
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = defaultSessionTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &server{
		logger:         o.Logger,
		pool:           o.Pool,
		store:          o.Store,
		engine:         o.Engine,
		dispatcher:     o.Dispatcher,
		hub:            o.Hub,
		monReg:         o.MonitorRegistry,
		notifyReg:      o.NotifyRegistry,
		box:            o.SecretsBox,
		reporter:       o.Reporter,
		push:           o.PushRecorder,
		auditSink:      o.AuditSink,
		metrics:        o.Metrics,
		metricsToken:   o.MetricsToken,
		logos:          o.Logos,
		avatars:        o.Avatars,
		mailer:         o.Mailer,
		wsPub:          o.WSPublisher,
		maint:          o.Maintenance,
		baseURL:        o.PublicBaseURL,
		allowedOrigins: o.AllowedOrigins,
		secureCookies:  o.SecureCookies,
		sessionSecret:  o.SessionSecret,
		sessionTTL:     o.SessionTTL,
		trustedProxies: o.TrustedProxies,
		now:            o.Now,
		loginLimiter:   newRateLimiter(loginRateLimit, loginRateWindow),
		resetLimiter:   newRateLimiter(resetPerAddressLimit, resetPerAddressWindow),
		publicLimiter:  newRateLimiter(publicRateLimit, publicRateWindow),
		pushLimiter:    newRateLimiter(pushTokenRateLimit, pushRateWindow),
		pushIPLimiter:  newRateLimiter(pushIPRateLimit, pushRateWindow),
		lockout:        newLockout(lockoutThreshold, lockoutDuration),
	}

	if len(o.OIDC) > 0 {
		s.oidc = make(map[string]*OIDCProvider, len(o.OIDC))
		for _, p := range o.OIDC {
			s.oidc[p.ID] = p
		}
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(s.realIP)
	r.Use(middleware.Compress(5))
	r.Use(requestLogger(o.Logger))
	r.Use(recoverer(o.Logger))
	r.Use(securityHeaders)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	// Prometheus scrape endpoint. Closed by default: without a configured scrape
	// token the endpoint answers 404 rather than serving metrics to anyone, so a
	// deployment that never set a token does not leak its internals. With a token
	// the wired handler serves and enforces the token itself.
	if s.metrics != nil {
		if s.metricsToken == "" {
			r.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeProblem(w, http.StatusNotFound, "Not Found")
			}))
		} else {
			r.Handle("/metrics", s.metrics)
		}
	}

	r.Route("/api", func(api chi.Router) {
		api.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeProblem(w, http.StatusNotFound, "Not Found")
		})
		api.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		})
		api.Route("/v1", func(v1 chi.Router) {
			v1.Get("/version", s.handleVersion)
			// The spec describes route shapes only, never instance data, so it
			// sits beside /version: inside /api/v1 but outside both the Store
			// guard and the auth group, which is what makes it reachable from
			// curl, Postman and client generators.
			v1.Get("/openapi.json", s.handleOpenAPISpec)
			// The human-readable form of the same contract. It renders the
			// document above by fetching it in the browser, so it is public for
			// the same reasons and is registered beside it rather than at a
			// top-level /docs, which the SPA catch-all owns.
			v1.Get("/docs", s.handleAPIDocs)
			if s.store != nil {
				s.mountV1(v1)
			}
		})
	})

	if s.hub != nil {
		r.Get("/ws", s.handleWS)
	}

	// Uploaded status-page logos are public assets: they are rendered by
	// anonymous visitors of a public status page, so this route sits outside the
	// auth group. It must also precede the SPA catch-all below, which answers any
	// unmatched path with index.html and HTTP 200 (spa.go), so a route mounted
	// after it would be unreachable and a broken logo would look like a success.
	if s.logos != nil {
		r.Get(logoURLPrefix+"{name}", s.handleServeLogo)
	}

	// Avatars are served from their own URL space, backed by their own directory.
	// Same placement rule as the logo route: it must precede the SPA catch-all
	// below or a missing avatar would render as index.html with a 200.
	if s.avatars != nil {
		r.Get(avatarURLPrefix+"{name}", s.handleServeAvatar)
	}

	// Public status pages (the SPA's /status/[slug] routes) stay framable by
	// anyone -- they were before this app sent any frame-ancestors header, and
	// sites embedding them must not break. The same bundle is served either way;
	// only the header differs. This must precede the SPA catch-all below, which
	// answers any unmatched path with index.html and HTTP 200 (spa.go) and would
	// otherwise swallow it, serving the status page with frame-ancestors 'none'.
	r.With(framableStatusPage).Handle("/status/*", o.Static)

	r.Handle("/*", o.Static)
	return r, s
}

// mountV1 wires the authenticated REST surface. It is only reached when a Store
// is configured.
func (s *server) mountV1(v1 chi.Router) {
	// Bootstrap + auth (mostly public, self-rate-limited).
	//
	// The setup pair is rate-limited like the other public endpoints rather than
	// left bare. It cannot be used to add accounts once one exists — handleSetup
	// answers 409 after a single CountUsers — but it is unauthenticated, it is
	// reachable before anybody has logged in, and POST carries a body that gets
	// decoded and validated. A budget costs nothing here: the wizard asks once on
	// load, far below the public allowance.
	v1.Group(func(pub chi.Router) {
		pub.Use(s.publicRateLimit)
		pub.Get("/setup", s.handleSetupStatus)
		pub.Post("/setup", s.handleSetup)
	})
	v1.Post("/auth/login", s.handleLogin)
	// CSRF-protected like every other session-authenticated mutation: it
	// deletes the session row, so without it any site could sign a user out.
	// It was the one literal exception to the invariant.
	v1.With(s.csrf).Post("/auth/logout", s.handleLogout)

	// Password recovery. Both are public by necessity — the caller cannot log in
	// — and both are POST with a body, so neither can sit behind the CSRF
	// middleware: there is no session to double-submit against. They rate-limit
	// themselves on the login limiter instead.
	v1.Post("/auth/password-reset", s.handleRequestPasswordReset)
	v1.Post("/auth/password-reset/confirm", s.handleConfirmPasswordReset)

	// Provider discovery is always mounted, even with SSO off. Its whole job is
	// to answer "is single sign-on available here", and the honest answer is an
	// empty list — a 404 is indistinguishable from a broken deployment, and the
	// login screen asks on every load, so an unconfigured instance would log an
	// error for the common case.
	v1.Group(func(pub chi.Router) {
		pub.Use(s.publicRateLimit)
		pub.Get("/auth/oidc/providers", s.handleOIDCProviders)
	})

	// The flow endpoints stay conditional: with no provider configured they
	// could only ever report an error, so an unconfigured instance has no SSO
	// surface to probe.
	if len(s.oidc) > 0 {
		v1.Group(func(pub chi.Router) {
			pub.Use(s.publicRateLimit)
			pub.Get("/auth/oidc/{provider}/start", s.handleOIDCStart)
			// authenticate, never requireAuth: the same callback serves both the
			// anonymous login mode and the link mode, and only the latter has a
			// principal. authenticate identifies without rejecting.
			pub.With(s.authenticate).Get("/auth/oidc/{provider}/callback", s.handleOIDCCallback)
		})
	}

	// Public push heartbeat endpoint (passive monitors). POST only: a heartbeat
	// is a state-changing report, and a GET form could be tripped by a link
	// prefetcher, crawler or <img> tag and record spurious beats. The push agent
	// owns the token lifecycle; this route only fixes the method.
	v1.Post("/push/{token}", s.handlePush)

	// Public, embeddable SVG badges. Addressed by an opaque, revocable badge
	// token (never the monitor's sequential id) so they cannot be walked to
	// enumerate every monitor, and rate-limited like the other public endpoints.
	v1.Group(func(pub chi.Router) {
		pub.Use(s.publicRateLimit)
		pub.Get("/badge/{token}/status.svg", s.handleStatusBadge)
		pub.Get("/badge/{token}/uptime.svg", s.handleUptimeBadge)
	})

	// Public status pages (unauthenticated). The bare /public/status route
	// resolves the page from the request Host header (custom-domain routing);
	// the {slug} routes address a page directly.
	v1.Group(func(pub chi.Router) {
		pub.Use(s.publicRateLimit)
		pub.Get("/public/status", s.handlePublicStatusPage)
		pub.Get("/public/status/{slug}", s.handlePublicStatusPage)
		pub.Get("/public/status/{slug}/badge.svg", s.handlePublicPageBadge)
		// The availability archive. Only the {slug} form exists — a static
		// /public/status/history would compete with {slug} and make a page
		// actually named "history" unreachable. See handlePublicStatusHistory.
		pub.Get("/public/status/{slug}/history", s.handlePublicStatusHistory)
	})

	// Registry listings are read-only metadata; require any authenticated
	// principal so the config forms are gated with the rest of the UI.
	v1.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		r.Use(s.requireAuth)

		r.Get("/registry/monitor-types", s.handleMonitorTypes)
		r.Get("/registry/notification-types", s.handleNotificationTypes)

		// Self-service profile.
		r.Get("/me", s.handleMe)
		r.Get("/me/sessions", s.handleListSessions)
		r.With(s.requireSession, s.csrf).Delete("/me/sessions/{id}", s.handleRevokeSession)
		r.With(s.requireSession, s.csrf).Put("/me/password", s.handleChangePassword)
		r.With(s.requireSession, s.csrf).Put("/me/email", s.handleChangeEmail)
		// Avatars sit here, in the plain authenticate+requireAuth group, and NOT
		// under requireRole(roleWriter) the way the status-page logo endpoints
		// do. A viewer owns their own profile, and the write touches only their
		// own row.
		r.With(s.requireWriteScope, s.csrf).Post("/me/avatar", s.handleUploadAvatar)
		r.With(s.requireWriteScope, s.csrf).Delete("/me/avatar", s.handleDeleteAvatar)

		// Linked external identities. Linking starts with a CSRF-protected POST
		// that returns the authorize URL for the browser to navigate to, rather
		// than a GET redirect: a top-level GET cannot carry the CSRF header, and
		// linking an identity to the caller's account is a mutation.
		if len(s.oidc) > 0 {
			r.Get("/me/oidc/identities", s.handleListIdentities)
			r.With(s.requireSession, s.csrf).Post("/me/oidc/{provider}/link-start", s.handleOIDCLinkStart)
			r.With(s.requireSession, s.csrf).Delete("/me/oidc/identities/{id}", s.handleUnlinkIdentity)
		}

		// 2FA enrolment/management.
		r.With(s.requireSession, s.csrf).Post("/me/totp/enroll", s.handleTOTPEnroll)
		r.With(s.requireSession, s.csrf).Post("/me/totp/verify", s.handleTOTPVerify)
		r.With(s.requireSession, s.csrf).Post("/me/totp/disable", s.handleTOTPDisable)
		r.With(s.requireSession, s.csrf).Post("/me/totp/backup-codes", s.handleRegenerateBackupCodes)

		// API keys.
		r.Get("/me/api-keys", s.handleListAPIKeys)
		r.With(s.requireSession, s.csrf).Post("/me/api-keys", s.handleCreateAPIKey)
		r.With(s.requireSession, s.csrf).Delete("/me/api-keys/{id}", s.handleRevokeAPIKey)

		// Monitors.
		r.Get("/monitors", s.handleListMonitors)
		// The recycle bin. Registered before /monitors/{id} for legibility, not
		// out of necessity: chi's trie prefers a STATIC segment over a wildcard
		// whatever order they were added in, so "trash" reaches this handler
		// either way. (The order that does matter is against the SPA catch-all
		// at the end of the tree — see the note on r.Handle("/*").)
		r.With(s.requireRole(roleWriter)).Get("/monitors/trash", s.handleListDeletedMonitors)
		// Every live monitor's state transitions in one paged log. A static
		// segment like "trash", so /monitors/{id} cannot shadow it.
		r.Get("/monitors/events", s.handleListEvents)
		r.Get("/monitors/{id}", s.handleGetMonitor)
		r.Get("/monitors/{id}/heartbeats", s.handleMonitorHeartbeats)
		r.Get("/monitors/{id}/status", s.handleMonitorStatus)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleWriter))
			w.Use(s.csrf)
			w.Post("/monitors", s.handleCreateMonitor)
			w.Put("/monitors/{id}", s.handleUpdateMonitor)
			w.Delete("/monitors/{id}", s.handleDeleteMonitor)
			// Restoring is an ordinary edit with a way back, so a writer may do
			// it. Purging is not, and is gated to admin below.
			w.Post("/monitors/{id}/restore", s.handleRestoreMonitor)
			w.Post("/monitors/{id}/pause", s.handlePauseMonitor)
			w.Post("/monitors/{id}/resume", s.handleResumeMonitor)
			w.Post("/monitors/{id}/check", s.handleCheckMonitor)
			w.Post("/monitors/{id}/push-token", s.handleRegeneratePushToken)
			// Embeddable badge lifecycle: mint/return the opaque badge token, or
			// revoke it. The public /badge/{token}/*.svg endpoints resolve it.
			w.Post("/monitors/{id}/badge", s.handleCreateMonitorBadge)
			w.Delete("/monitors/{id}/badge", s.handleRevokeMonitorBadge)
			// One request, one action, an explicit id list. Registered before
			// nothing in particular — there is no POST /monitors/{id}, so the
			// static "bulk" segment cannot be shadowed.
			w.Post("/monitors/bulk", s.handleBulkMonitors)
		})

		// Monitor groups: organisational folders for the monitor list.
		r.Get("/monitor-groups", s.handleListMonitorGroups)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleWriter))
			w.Use(s.csrf)
			w.Post("/monitor-groups", s.handleCreateMonitorGroup)
			w.Put("/monitor-groups/{id}", s.handleUpdateMonitorGroup)
			w.Delete("/monitor-groups/{id}", s.handleDeleteMonitorGroup)
		})

		// Reporting & analytics (read-only).
		r.Get("/monitors/{id}/uptime", s.handleMonitorUptime)
		r.Get("/monitors/{id}/report", s.handleMonitorReport)
		r.Get("/report/flakiest", s.handleFlakiest)
		r.Get("/report/certificates", s.handleCertOverview)
		r.Get("/report/sla.csv", s.handleSLACSV)
		r.Get("/report/sla.pdf", s.handleSLAPDF)

		// Config export/import and Uptime-Kuma import (admin).
		r.With(s.requireRole(roleAdmin)).Get("/export", s.handleExport)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleAdmin))
			w.Use(s.csrf)
			w.Post("/import", s.handleImport)
			w.Post("/import/uptime-kuma", s.handleImportKuma)
		})

		// Tags.
		r.Get("/tags", s.handleListTags)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleWriter))
			w.Use(s.csrf)
			w.Post("/tags", s.handleCreateTag)
			w.Put("/tags/{id}", s.handleUpdateTag)
			w.Delete("/tags/{id}", s.handleDeleteTag)
		})

		// Notifications.
		r.Get("/notifications", s.handleListNotifications)
		r.Get("/notifications/{id}", s.handleGetNotification)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleWriter))
			w.Use(s.csrf)
			w.Post("/notifications", s.handleCreateNotification)
			w.Put("/notifications/{id}", s.handleUpdateNotification)
			w.Delete("/notifications/{id}", s.handleDeleteNotification)
			w.Post("/notifications/{id}/test", s.handleTestNotification)
		})

		// Maintenance windows.
		r.Get("/maintenance", s.handleListMaintenance)
		r.Get("/maintenance/{id}", s.handleGetMaintenance)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleWriter))
			w.Use(s.csrf)
			w.Post("/maintenance", s.handleCreateMaintenance)
			w.Put("/maintenance/{id}", s.handleUpdateMaintenance)
			w.Delete("/maintenance/{id}", s.handleDeleteMaintenance)
		})

		// Status pages: CRUD plus incident management. The public rendering
		// lives on the unauthenticated /public routes above.
		r.Get("/status-pages", s.handleListStatusPages)
		r.Get("/status-pages/{id}", s.handleGetStatusPage)
		r.Get("/status-pages/{id}/incidents", s.handleListIncidents)
		r.Get("/status-pages/{id}/incidents/{incID}", s.handleGetIncident)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleWriter))
			w.Use(s.csrf)
			w.Post("/status-pages", s.handleCreateStatusPage)
			w.Put("/status-pages/{id}", s.handleUpdateStatusPage)
			w.Delete("/status-pages/{id}", s.handleDeleteStatusPage)
			w.Post("/status-pages/{id}/incidents", s.handleCreateIncident)
			w.Put("/status-pages/{id}/incidents/{incID}", s.handleUpdateIncident)
			w.Delete("/status-pages/{id}/incidents/{incID}", s.handleDeleteIncident)
			w.Post("/status-pages/{id}/incidents/{incID}/updates", s.handleAddIncidentUpdate)
			w.Put("/status-pages/{id}/incidents/{incID}/postmortem", s.handleSetIncidentPostmortem)
			w.Post("/status-pages/{id}/logo", s.handleUploadStatusPageLogo)
			w.Delete("/status-pages/{id}/logo", s.handleDeleteStatusPageLogo)
			w.Patch("/status-pages/{id}/layout", s.handleReorderStatusPageLayout)
		})

		// Purging a binned monitor destroys its whole history and cannot be
		// undone. It is the one monitor operation gated to admin: the bin exists
		// so that the irreversible step is separate and deliberate, and it should
		// be taken by someone who can also reach the backups.
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleAdmin))
			w.Use(s.csrf)
			w.Delete("/monitors/{id}/purge", s.handlePurgeMonitor)
		})

		// Settings (admin).
		r.With(s.requireRole(roleAdmin)).Get("/settings", s.handleListSettings)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleAdmin))
			w.Use(s.csrf)
			w.Put("/settings/{key}", s.handleSetSetting)
		})

		// User administration (admin).
		r.With(s.requireRole(roleAdmin)).Get("/users", s.handleListUsers)
		r.With(s.requireRole(roleAdmin)).Get("/audit-log", s.handleListAudit)
		// The CSV export is a GET so a browser can download it from an <a
		// download>, like the SLA report and the config export. It is admin-only
		// for the same reason the JSON list is: the audit log names who did what
		// from which address.
		r.With(s.requireRole(roleAdmin)).Get("/audit-log.csv", s.handleExportAuditCSV)
		r.Group(func(w chi.Router) {
			w.Use(s.requireRole(roleAdmin))
			w.Use(s.csrf)
			w.Post("/users", s.handleCreateUser)
			w.Put("/users/{id}", s.handleUpdateUser)
			w.Delete("/users/{id}", s.handleDeleteUser)
		})
	})
}
