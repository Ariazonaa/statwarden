// Command statwarden is the single self-hosted binary: it loads configuration,
// connects to PostgreSQL, applies embedded migrations, and serves the REST
// API plus the embedded SvelteKit SPA on one port with graceful shutdown.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	statwarden "github.com/Ariazonaa/statwarden"
	"github.com/Ariazonaa/statwarden/internal/anomaly"
	"github.com/Ariazonaa/statwarden/internal/api"
	"github.com/Ariazonaa/statwarden/internal/backup"
	"github.com/Ariazonaa/statwarden/internal/config"
	"github.com/Ariazonaa/statwarden/internal/db"
	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/maintenance"
	"github.com/Ariazonaa/statwarden/internal/metrics"
	"github.com/Ariazonaa/statwarden/internal/migrations"
	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
	"github.com/Ariazonaa/statwarden/internal/monitor/push"
	"github.com/Ariazonaa/statwarden/internal/monitor/sslcheck"
	"github.com/Ariazonaa/statwarden/internal/notify"
	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
	"github.com/Ariazonaa/statwarden/internal/outbound"
	"github.com/Ariazonaa/statwarden/internal/report"
	"github.com/Ariazonaa/statwarden/internal/retention"
	"github.com/Ariazonaa/statwarden/internal/safego"
	"github.com/Ariazonaa/statwarden/internal/siem"
	"github.com/Ariazonaa/statwarden/internal/version"
	"github.com/Ariazonaa/statwarden/internal/ws"

	// Monitor types self-register with the checker registry.
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/certcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/dnscheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/dockercheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/grpccheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/httpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/mongocheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/mqttcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/mysqlcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/ntpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/pingcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/postgrescheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/pushcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/radiuscheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/redischeck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/snmpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/steamcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/tcpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/udpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/websocketcheck"

	// Notification providers self-register with the notifier registry.
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/apprise"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/discord"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/googlechat"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/gotify"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/matrix"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/mattermost"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/ntfy"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/opsgenie"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/pagerduty"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/pushover"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/rocketchat"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/signal"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/slack"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/smtp"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/teams"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/telegram"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/twilio"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/webhook"
	_ "github.com/Ariazonaa/statwarden/internal/notify/providers/webpush"
)

const (
	dbConnectTimeout = 10 * time.Second
	shutdownTimeout  = 15 * time.Second
	// pushWatchdogInterval is how often overdue push monitors are swept.
	pushWatchdogInterval = 30 * time.Second
	// rollupInterval is how often the hourly/daily heartbeat rollups run.
	rollupInterval = time.Hour
	// notifySendAttempts and notifyRetryDelay bound one notification delivery.
	// An alert is the product, so a provider that blips for a few seconds must
	// not cost one; three attempts five seconds apart stays well inside the
	// dispatcher's own send timeout budget.
	notifySendAttempts = 3
	notifyRetryDelay   = 5 * time.Second
	// statusPageResolveTimeout bounds the lookup that maps a monitor to the
	// public status pages showing it. It runs per state transition, off the
	// engine's event path; failing fast simply skips that one live poke.
	statusPageResolveTimeout = 3 * time.Second

	// HTTP server timeouts. See the http.Server literal for why the read and
	// write budgets are minutes rather than seconds.
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 5 * time.Minute
	httpWriteTimeout      = 5 * time.Minute
	httpIdleTimeout       = 2 * time.Minute
)

// settingString reads a settings key whose value is a JSON string, returning ""
// when the key is absent or not a string. Used for the outbound webhook URL and
// the metrics token.
func settingString(ctx context.Context, store api.Store, key string) (string, error) {
	s, err := store.GetSetting(ctx, key)
	if err != nil {
		// A key that was never set is not a failure: it is the ordinary state of
		// an integration nobody configured, and the callers read it as "off".
		if errors.Is(err, api.ErrNotFound) {
			return "", nil
		}
		// Everything else IS a failure, and it must not be flattened into "".
		// It used to be, and the two callers here both interpret "" as "no
		// target configured" — so a database that was briefly unreachable
		// silently disabled the outbound webhook and the audit feed for exactly
		// as long as it was unreachable, with nothing in the log about it.
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	var v string
	if err := json.Unmarshal(s.Value, &v); err != nil {
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	return v, nil
}

// settingWebhookURL reads one of the two webhook URL settings and applies the
// read-path clamp before handing it to a delivery queue.
//
// api.settingValidators checks these on write, but the settings table is also
// writable directly in psql, so every validated setting in that file has a
// matching clamp where it is READ. These two are read here. A stored value that
// cannot work comes back as an error rather than as a URL, so it is reported
// once by the queue instead of failing a POST per event.
func settingWebhookURL(ctx context.Context, store api.Store, key string) (string, error) {
	v, err := settingString(ctx, store, key)
	if err != nil {
		return "", err
	}
	if err := api.CheckWebhookURL(v); err != nil {
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	return v, nil
}

// settingWebhookSecret reads one of the two webhook signing secrets: opened with
// the secrets box, since the API seals them on write, then clamped like the URLs.
//
// A secret that cannot be opened is an error, never "": the queue drops the
// payload on an error, whereas "" would send it unsigned — quietly removing the
// authentication the operator configured the moment ENCRYPTION_KEY is wrong.
func settingWebhookSecret(ctx context.Context, store api.Store, box *secrets.Box, key string) (string, error) {
	st, err := store.GetSetting(ctx, key)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	raw := st.Value
	if box != nil {
		if raw, err = box.DecryptConfig(raw); err != nil {
			return "", fmt.Errorf("setting %q: cannot decrypt it (check ENCRYPTION_KEY): %w", key, err)
		}
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	if err := api.CheckWebhookSecret(v); err != nil {
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	return v, nil
}

// runRollups pre-aggregates heartbeats into hourly/daily buckets on a ticker,
// until ctx is cancelled.
//
// Each pass is contained on its own: safego.Go wraps this loop, but it recovers
// ABOVE the loop and the goroutine then ends, so one panic used to stop rollups
// for the life of the process. Nothing surfaces that — long-range reads quietly
// fall back to scanning raw heartbeats and only get slower.
// The first pass runs immediately rather than an hour in, matching the
// certificate sweeper, the outbox worker and the retention pruner. Rollups are
// pure aggregation over past heartbeats — nothing is alerted on and nothing is
// deleted — so the only thing the old delay bought was an hour of long-range
// reads falling back to scanning raw beats after every restart.
func runRollups(ctx context.Context, svc *report.Service, logger *slog.Logger) {
	ticker := time.NewTicker(rollupInterval)
	defer ticker.Stop()
	pass := func() {
		if err := svc.RunRollup(ctx); err != nil {
			logger.Error("heartbeat rollup job failed", "error", err)
		}
	}
	// The safego.Do calls stay literal at both call sites rather than being
	// folded into pass. It reads as duplication and is not: it mirrors
	// retention.loop, and the enforcement test looks for containment INSIDE the
	// loop body, which a helper closure hides from it. A guard that can be
	// satisfied by moving code out of view is not a guard.
	_ = safego.Do(logger, "heartbeat rollup pass", pass)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = safego.Do(logger, "heartbeat rollup pass", pass)
		}
	}
}

// newAnomalyDetector builds the response-time anomaly detector for the engine, or
// nil when detection is switched off — which is what the engine's own guard
// tests, so a disabled detector costs nothing.
//
// This is the wiring that was missing outright. The detector, the engine hook,
// the domain payload and the whole ANOMALY_* config block all existed and were
// unit-tested, and nothing ever constructed a Detector outside tests: the
// composition root simply never passed one, so ANOMALY_DETECTION_ENABLED=true
// validated the value and then did nothing, while .env.example and the README
// both advertised the alerts. It is a function rather than an inline block so a
// test can pin that an enabled configuration really does produce a detector
// carrying the configured thresholds.
func newAnomalyDetector(c config.AnomalyConfig, logger *slog.Logger) *anomaly.Detector {
	if !c.Enabled {
		return nil
	}
	logger.Info("response-time anomaly detection enabled",
		"sensitivity", c.Sensitivity, "min_samples", c.MinSamples, "consecutive", c.Consecutive)
	return anomaly.New(anomaly.Config{
		Sensitivity: c.Sensitivity,
		MinSamples:  c.MinSamples,
		Consecutive: c.Consecutive,
	})
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var migrateOnly bool
	flag.BoolVar(&migrateOnly, "migrate-only", false, "apply pending database migrations and exit")
	flag.Parse()

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		// The default logger is still in place here; the error text carries
		// the full list of configuration problems.
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	// Non-fatal configuration complaints. They are emitted here rather than
	// returned as an error because refusing to boot over a weak-but-working
	// secret would take the monitoring system down on the release that adds the
	// check, with no fix that preserves already-encrypted data.
	for _, warning := range cfg.Warnings {
		logger.Warn("configuration warning", "detail", warning)
	}

	logger.Info("starting statwarden",
		"version", version.Version,
		"commit", version.Commit,
		"build_date", version.Date,
		"dev_mode", cfg.DevMode,
	)

	if migrateOnly {
		if cfg.DatabaseURL == "" {
			return errors.New("DATABASE_URL is required for --migrate-only")
		}
		v, err := migrations.Run(cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("migrations: %w", err)
		}
		logger.Info("migrations applied", "schema_version", v)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connectDatabase(ctx, cfg, logger)
	if err != nil {
		return err
	}
	if pool != nil {
		defer pool.Close()
	}

	// WebSocket hub for the live dashboard. Always present; it is fed by the
	// engine below when a database is configured. Its status-page resolver is
	// wired once the store exists, a few lines down.
	// The two webhook delivery queues run on their OWN context, cancelled at the
	// very end of shutdown rather than by the signal.
	//
	// Their loop reacts to cancellation by draining once — a single
	// non-blocking sweep — and then refusing further work. Rooted in the process
	// context, that drain fired in microseconds against an empty queue while the
	// engine and the HTTP server had not even started stopping. Everything they
	// produced afterwards, which is precisely the traffic a graceful shutdown
	// exists to finish (the last state transitions, the audit rows of in-flight
	// requests), was then rejected as "queued after the drain".
	queueCtx, stopQueues := context.WithCancel(context.Background())
	defer stopQueues()

	hub := ws.NewHub(logger)

	// Notification secrets box: encrypts channel configs at rest and is used
	// both by the dispatcher and the REST API. Nil in DEV_MODE without a key.
	var box *secrets.Box
	if cfg.EncryptionKey != "" {
		box, err = secrets.New(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("notification secrets: %w", err)
		}
	}

	// Persistence for the REST API: PostgreSQL in production, an in-memory
	// store in DEV_MODE without a database so the API is still exercisable.
	var store api.Store
	if pool != nil {
		store = api.NewPgStore(pool)
	} else {
		store = api.NewMemStore()
		logger.Warn("REST API using in-memory store: data is not persisted (DEV_MODE)")
	}

	// Teach the hub which public status pages a monitor appears on. Without this
	// the hub's resolver stays nil and a monitor transition never reaches
	// status-page:{slug} — the topic the public page actually listens on — so
	// those pages only ever refreshed on an incident or a layout change, never
	// on the outage they exist to announce. The hub calls this off the engine's
	// event path on its own goroutine, so the query does not hold up a check.
	hub.SetPageResolver(func(monitorID int64) []string {
		ctx, cancel := context.WithTimeout(context.Background(), statusPageResolveTimeout)
		defer cancel()
		slugs, err := store.StatusPageSlugsForMonitor(ctx, monitorID)
		if err != nil {
			logger.Error("could not resolve status pages for monitor",
				"monitor_id", monitorID, "error", err)
			return nil
		}
		return slugs
	})

	// Maintenance windows: one evaluator shared by the engine (which suppresses
	// alerts with it) and the REST API (which refreshes it after every window
	// mutation and renders the public status page from it). Built over whichever
	// store is in use, so DEV_MODE behaves the same way.
	maintProvider := maintenance.NewProvider(maintenance.ProviderOptions{
		Loader: api.NewMaintenanceLoader(store, logger),
		Logger: logger,
	})
	maintProvider.Start(ctx)

	// Monitor engine, notification dispatcher, reporting, push and daily
	// certificate-expiry sweep. Without a database (DEV_MODE) there is nothing
	// to schedule or notify, so these stay idle and the app still starts.
	var (
		eng            *engine.Engine
		dispatcher     *notify.Dispatcher
		reportSvc      *report.Service
		pushRec        *push.Recorder
		metricsHandler http.Handler
		metricsToken   string
		// Declared out here so shutdown can join its delivery goroutine. Its
		// drain POSTs to a URL it reads from settings, so it needs the pool that
		// is closed on the way out of this function.
		outboundHook *outbound.Webhook
	)
	if pool != nil {
		dispatcher = notify.New(notify.Options{
			Logger: logger,
			Store:  notify.NewPgStore(pool, box, logger),
			// Without these the dispatcher defaults to a single attempt with no
			// pause, so one transient provider failure — an SMTP blip, a webhook
			// 502 — loses the alert outright and leaves only a log line.
			SendAttempts: notifySendAttempts,
			RetryDelay:   notifyRetryDelay,
		})
		dispatcher.Start()
		// Adopt outages that were already in progress when this process started.
		// The dispatcher tracks outages in memory only, so without this a restart
		// mid-outage silently swallows the eventual recovery alert (and every
		// resend reminder) — see Dispatcher.Restore.
		if err := dispatcher.Restore(ctx); err != nil {
			logger.Error("could not adopt in-progress outages", "error", err)
		}
		// Retry events that never finished delivering. Its first sweep is the
		// crash-recovery path: whatever the previous process was still holding
		// when it died is simply an unfinished row.
		safego.Go(logger, "outbox worker", func() { dispatcher.RunOutbox(ctx, notify.DefaultOutboxInterval) })

		// Generic outbound webhook: a settings-configured URL that receives a
		// JSON POST on every state change, read live so a settings change takes
		// effect immediately.
		// Started with the process context so the delivery goroutine ends with
		// the process and gets a chance to flush what it is holding. Left to
		// start itself lazily it would run on context.Background and simply
		// vanish at exit, taking its queue with it.
		outboundHook = outbound.New(outbound.Options{
			Logger: logger,
			URL: func(c context.Context) (string, error) {
				return settingWebhookURL(c, store, api.SettingOutboundWebhookURL)
			},
			Secret: func(c context.Context) (string, error) {
				return settingWebhookSecret(c, store, box, api.SettingOutboundWebhookSecret)
			},
		})

		outboundHook.Run(queueCtx)

		// Fan every transition out to the alert dispatcher, the live WebSocket
		// dashboard and the generic outbound webhook. Used by the engine and the
		// push subsystem alike.
		onStateChange := func(sc domain.StateChange) {
			dispatcher.HandleStateChange(sc)
			hub.PublishStateChange(sc)
			outboundHook.HandleStateChange(sc)
		}

		// The engine and the push subsystem read monitors.config straight from the
		// database, where the REST API stores it SEALED. Without the opener the
		// checker validation rejects the envelope and engine.Start skips the
		// monitor, so anything created or edited since at-rest encryption landed
		// quietly stopped being checked at the next restart.
		var configOpener *secrets.Box // typed nil is not a nil interface, so keep it concrete
		if box != nil {
			configOpener = box
		}
		var engineOpener engine.ConfigOpener
		var pushOpener push.ConfigOpener
		if configOpener != nil {
			engineOpener, pushOpener = configOpener, configOpener
		}

		eng = engine.New(engine.Options{
			Logger:      logger,
			Store:       engine.NewPgStore(pool, engineOpener, logger),
			Workers:     cfg.EffectiveWorkerPoolSize(),
			OnEvent:     onStateChange,
			OnHeartbeat: hub.PublishHeartbeat,
			Maintenance: maintProvider,
			Anomaly:     newAnomalyDetector(cfg.AnomalyDetection, logger),

			FlapThreshold:       cfg.FlapThreshold,
			FlapWindow:          cfg.FlapWindow,
			DownBackoffAfter:    cfg.DownBackoffAfter,
			DownBackoffInterval: cfg.DownBackoffInterval,
		})
		if err := eng.Start(ctx); err != nil {
			return fmt.Errorf("monitor engine: %w", err)
		}

		// Push (passive) monitors: record incoming heartbeats and run a
		// watchdog that trips overdue monitors DOWN.
		pushStore := push.NewPgStore(pool, pushOpener, logger)
		// Suppress is the engine itself: push monitors never pass through the
		// scheduler's emit path, so without this hook their alerts ignore
		// maintenance windows and a DOWN parent entirely.
		pushOpts := push.Options{
			Store: pushStore, OnEvent: onStateChange, Suppress: eng, Logger: logger,
		}
		pushRec = push.NewRecorder(pushOpts)
		wd := push.NewWatchdog(pushOpts)
		safego.Go(logger, "push watchdog", func() { wd.Run(ctx, pushWatchdogInterval) })

		// Reporting service plus the periodic hourly/daily rollup job.
		reportSvc = report.NewService(report.NewPgStore(pool), nil)
		safego.Go(logger, "rollup job", func() { runRollups(ctx, reportSvc, logger) })
		// One backfill pass over the retention window. RunRollup only writes the
		// bucket that just closed, so heartbeats older than this job — an upgrade,
		// or a spell where it was down — would otherwise never get rollups, and
		// every long-range read would scan raw beats forever. Idempotent, and off
		// the startup path so it never delays serving.
		safego.Go(logger, "rollup backfill", func() {
			since := time.Now().AddDate(0, 0, -cfg.HeartbeatRetentionDays)
			if err := reportSvc.Backfill(ctx, since); err != nil {
				logger.Error("heartbeat rollup backfill failed", "error", err)
			} else {
				logger.Info("heartbeat rollup backfill complete", "since", since)
			}
		})

		// Prometheus /metrics endpoint. The scrape token is read once and handed to
		// both the handler (which enforces it) and the router (which keeps the
		// endpoint closed entirely when no token is set, rather than open).
		// A read that FAILS leaves the token empty, which keeps /metrics closed.
		// That is the safe direction and the reason this one does not abort
		// startup — but it is also indistinguishable from "no token configured"
		// to anyone looking at the running server, so it is said out loud here.
		var tokErr error
		metricsToken, tokErr = settingString(ctx, store, "metrics_token")
		if tokErr != nil {
			logger.Error("cannot read the metrics scrape token; /metrics stays closed", "error", tokErr)
		}
		metricsHandler = metrics.Handler(
			newMetricsSource(store, eng, cfg.EffectiveWorkerPoolSize()),
			metricsToken,
		)

		sweeper := sslcheck.NewSweeper(sslcheck.SweeperOptions{
			Source: sslcheck.NewPgCertSource(pool),
			Logger: logger,
			OnEvent: func(e sslcheck.Event) {
				dispatcher.HandleCertExpiry(notify.CertExpiryEvent{
					MonitorID:     e.MonitorID,
					MonitorName:   e.MonitorName,
					Issuer:        e.Issuer,
					ValidTo:       e.ValidTo,
					DaysRemaining: e.DaysRemaining,
					Expired:       e.Expired,
				})
			},
		})
		safego.Go(logger, "certificate sweeper", func() { sweeper.Run(ctx) })

		// Retention: prune heartbeats older than HEARTBEAT_RETENTION_DAYS, the
		// delivery log older than DELIVERY_RETENTION_DAYS, and the monitor
		// recycle bin older than TRASH_RETENTION_DAYS. One job, three windows —
		// the tables answer different questions and are kept for different
		// reasons. Self-contained goroutine that stops with ctx.
		retention.New(retention.Options{
			Store:                 retention.NewPgStore(pool),
			RetentionDays:         cfg.HeartbeatRetentionDays,
			DeliveryRetentionDays: cfg.DeliveryRetentionDays,
			TrashRetentionDays:    cfg.TrashRetentionDays,
			// The runtime override the settings screen writes. Without this the
			// whole control was inert: validated, stored, displayed, and read by
			// nobody, while its help text promised an irreversible delete.
			DaysFunc: func(c context.Context) (int, error) {
				return api.RuntimeRetentionDays(c, store)
			},
			Logger: logger,
		}).Start(ctx)

		// Scheduled pg_dump backups, opt-in via BACKUP_ENABLED. Inside the
		// database branch because the job dumps that database; DEV_MODE has
		// nothing to dump.
		//
		// A failed preflight (no pg_dump on PATH, unwritable directory) disables
		// the job with an ERROR rather than aborting startup — see
		// backup.Runner.Start for why monitoring should not go down because
		// backups cannot run.
		if cfg.BackupEnabled {
			backup.New(backup.Options{
				DatabaseURL: cfg.DatabaseURL,
				Dir:         cfg.BackupDir,
				Interval:    time.Duration(cfg.BackupIntervalHours) * time.Hour,
				Keep:        cfg.BackupKeep,
				MaxAge:      time.Duration(cfg.BackupMaxAgeDays) * 24 * time.Hour,
				Logger:      logger,
			}).Start(ctx)
		}
	} else {
		logger.Warn("monitor engine and notification dispatcher idle: no database (DEV_MODE)")
	}

	var apiEngine api.Engine
	if eng != nil {
		apiEngine = eng
	}
	var apiDispatcher api.Dispatcher
	if dispatcher != nil {
		apiDispatcher = dispatcher
	}
	var apiBox api.SecretsBox
	if box != nil {
		apiBox = box
	}
	var apiReporter api.Reporter
	if reportSvc != nil {
		apiReporter = reportSvc
	}
	var apiPush api.PushRecorder
	if pushRec != nil {
		apiPush = pushRec
	}

	// Logo uploads are optional: if the directory cannot be created the app
	// still serves everything else and the upload endpoints report 503.
	var apiLogos *api.LogoStore
	if ls, err := api.NewLogoStore(cfg.UploadsDir); err != nil {
		logger.Warn("status-page logo uploads disabled", "dir", cfg.UploadsDir, "err", err)
	} else {
		apiLogos = ls
	}

	// Avatars get a directory of their own beneath the uploads root, never the
	// logo directory: two stores sharing one directory would share the
	// ".upload-*" temp namespace, and either serving route could then reach the
	// other's files, since the generated name shape is identical.
	avatarDir := filepath.Join(cfg.UploadsDir, "avatars")
	var apiAvatars *api.LogoStore
	if as, err := api.NewAvatarStore(avatarDir); err != nil {
		logger.Warn("avatar uploads disabled", "dir", avatarDir, "err", err)
	} else {
		apiAvatars = as
	}

	// Single sign-on is off unless it is configured. Nothing is mounted when
	// cfg.OIDC is nil, so an unconfigured instance has no SSO surface at all.
	var apiOIDC []*api.OIDCProvider
	if o := cfg.OIDC; o != nil {
		apiOIDC = append(apiOIDC, &api.OIDCProvider{
			ID:                  o.ProviderID,
			DisplayName:         o.DisplayName,
			Issuer:              o.Issuer,
			ClientID:            o.ClientID,
			ClientSecret:        o.ClientSecret,
			Scopes:              o.Scopes,
			AutoProvision:       o.AutoProvision,
			DefaultRole:         o.DefaultRole,
			AllowedEmailDomains: o.AllowedEmailDomains,
			AllowInsecureIssuer: o.AllowInsecureIssuer,
		})
		// The client secret is never logged, here or anywhere else.
		logger.Info("single sign-on enabled",
			"provider", o.ProviderID, "issuer", o.Issuer, "auto_provision", o.AutoProvision)
	}

	// Transactional mail: a configured SMTP server delivers for real; otherwise
	// the log-only mailer is the EXPLICIT development fallback. The affected flow
	// (currently the password-reset mail) still runs either way — the reset
	// endpoint keeps its enumeration-safe 202 — but with no SMTP configured the
	// operator gets a loud warning rather than mail silently vanishing while every
	// response claims it was delivered.
	var mailer api.Mailer
	if cfg.SMTP.Configured() {
		mailer = api.NewSMTPMailer(api.SMTPSettings{
			Host:       cfg.SMTP.Host,
			Port:       cfg.SMTP.Port,
			Username:   cfg.SMTP.Username,
			Password:   cfg.SMTP.Password,
			From:       cfg.SMTP.From,
			Encryption: cfg.SMTP.Encryption,
		}, logger)
		// Credentials are never logged; host/port/encryption/sender are not secret.
		logger.Info("SMTP mail delivery enabled",
			"host", cfg.SMTP.Host,
			"port", cfg.SMTP.EffectivePort(),
			"encryption", cfg.SMTP.Encryption,
			"from", cfg.SMTP.From,
		)
	} else {
		mailer = api.LogMailer{Logger: logger}
		logger.Warn("SMTP not configured: transactional mail (password reset) is only " +
			"LOGGED, not sent — set SMTP_HOST and SMTP_FROM to enable real delivery")
	}

	// One-shot re-seal of any TOTP secret still stored in plaintext.
	//
	// Migration 0013 could not do it — the key is in this process, never in the
	// database — and left each account's next write to re-seal its own secret.
	// An account with 2FA on that nobody touches has no next write, so without
	// this pass its second factor stays readable in a database dump forever.
	// Idempotent and quiet when there is nothing to do.
	if sealedCount, rerr := api.ResealTOTPSecrets(ctx, store, apiBox, logger); rerr != nil {
		logger.Error("could not re-seal legacy TOTP secrets", "err", rerr)
	} else if sealedCount > 0 {
		logger.Info("sealed TOTP secrets that predate at-rest encryption", "count", sealedCount)
	}

	// Audit-log SIEM feed: a settings-configured collector URL that receives one
	// JSON POST per audit entry, read live so a settings change takes effect
	// immediately.
	//
	// Unlike the outbound webhook this is wired even without a database. The audit
	// log is written in DEV_MODE too (MemStore), and an operator pointing a
	// collector at a dev instance to check their pipeline should see the same
	// events a production one emits.
	//
	// Started with the process context for the reason the outbound webhook is:
	// left to start itself lazily the delivery goroutine would run on
	// context.Background and simply vanish at exit, taking its queue with it.
	auditSink := siem.New(siem.Options{
		Logger: logger,
		URL: func(c context.Context) (string, error) {
			return settingWebhookURL(c, store, api.SettingAuditWebhookURL)
		},
		Secret: func(c context.Context) (string, error) {
			return settingWebhookSecret(c, store, box, api.SettingAuditWebhookSecret)
		},
		Instance: func(c context.Context) (string, error) {
			return settingString(c, store, "instance_name")
		},
	})
	auditSink.Run(queueCtx)

	handler := api.New(api.Options{
		Logger:         logger,
		Pool:           pool,
		Static:         statwarden.SPAHandler(),
		Store:          store,
		Engine:         apiEngine,
		Dispatcher:     apiDispatcher,
		Hub:            hub,
		SecretsBox:     apiBox,
		Reporter:       apiReporter,
		PushRecorder:   apiPush,
		AuditSink:      auditSink,
		Metrics:        metricsHandler,
		MetricsToken:   metricsToken,
		Logos:          apiLogos,
		Avatars:        apiAvatars,
		OIDC:           apiOIDC,
		Mailer:         mailer,
		WSPublisher:    hub,
		Maintenance:    maintProvider,
		PublicBaseURL:  cfg.PublicBaseURL,
		AllowedOrigins: cfg.AllowedOrigins,
		SecureCookies:  strings.HasPrefix(cfg.PublicBaseURL, "https://"),
		TrustedProxies: cfg.TrustedProxies,
		SessionTTL:     cfg.SessionTTL,
		SessionSecret:  cfg.SessionSecret,
	})

	// ReadHeaderTimeout alone only stopped a slowloris that dribbles HEADERS.
	// A connection that finished its headers and then trickled a request BODY
	// one byte at a time was held open indefinitely, so a few hundred POSTs
	// could occupy the server for as long as the attacker cared to.
	//
	// The read budget is generous rather than tight because one legitimate
	// request really is large: the Uptime-Kuma importer accepts a 128 MiB SQLite
	// database, which takes minutes over a slow link. Bounding it at all is what
	// matters — an attacker gets minutes, not forever.
	//
	// WriteTimeout covers handler execution as well as the write itself, so it
	// is sized for the slowest legitimate response (an SLA PDF over a large
	// fleet) rather than for a fast one. It is safe for the WebSocket: that
	// connection is hijacked, and gorilla sets its own read and write deadlines
	// on every operation, which replace these.
	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	serveErr := make(chan error, 1)
	safego.Go(logger, "http server", func() {
		logger.Info("http server listening", "addr", srv.Addr, "public_base_url", cfg.PublicBaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	})

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutdown signal received, draining connections", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if eng != nil {
		if err := eng.Stop(shutdownCtx); err != nil {
			logger.Error("monitor engine drain incomplete", "error", err)
		}
	}
	if dispatcher != nil {
		if err := dispatcher.Stop(shutdownCtx); err != nil {
			logger.Error("notification dispatcher drain incomplete", "error", err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown incomplete, forcing close", "error", err)
		_ = srv.Close()
	}
	// Only NOW are the queues told to stop, so everything the steps above
	// produced is still accepted, and the drain has something to drain.
	stopQueues()
	// Join the webhook delivery goroutines LAST, and before returning.
	//
	// Both flush what they are still holding when the context is cancelled, and
	// both need the database while they do it: the target URL is a settings
	// read. Nothing waited for them, and `defer pool.Close()` fires on the way
	// out of this function — so the flush that exists to stop a restart being a
	// silent gap in the feed was racing a closing pool and losing. They come
	// after srv.Shutdown for the same reason: an in-flight request can still
	// audit, and that event should make it into the queue before the drain.
	//
	// Both are bounded by shutdownCtx and by their own drain budget, so an
	// unresponsive collector delays the exit, it does not prevent it.
	if outboundHook != nil {
		_ = outboundHook.Wait(shutdownCtx)
	}
	_ = auditSink.Wait(shutdownCtx)
	logger.Info("shutdown complete")
	return nil
}

// connectDatabase opens the pgx pool and runs migrations. Outside DEV_MODE a
// missing or unreachable database is fatal; in DEV_MODE the server keeps
// running without a pool so static assets and /healthz stay testable.
func connectDatabase(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		// config.Load guarantees this only happens in DEV_MODE.
		logger.Warn("DATABASE_URL not set; running without a database (DEV_MODE)")
		return nil, nil
	}

	connectCtx, cancel := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancel()
	pool, err := db.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		if !cfg.DevMode {
			return nil, fmt.Errorf("database: %w", err)
		}
		logger.Warn("database unreachable; continuing without it (DEV_MODE)", "error", err)
		return nil, nil
	}

	v, err := migrations.Run(cfg.DatabaseURL)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrations: %w", err)
	}
	logger.Info("database ready", "schema_version", v)
	return pool, nil
}

// newLogger builds the process-wide slog logger from LOG_FORMAT / LOG_LEVEL.
func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
