// Package config loads and validates Statwarden's environment-based configuration.
//
// Every setting is documented in .env.example at the repository root. Outside
// DEV_MODE the three secrets (DATABASE_URL, SESSION_SECRET, ENCRYPTION_KEY)
// are mandatory and startup fails loudly without them; with DEV_MODE=true they
// may be omitted so the binary can be smoke-tested without infrastructure.
//
// SESSION_SECRET and ENCRYPTION_KEY additionally have to be strong, not merely
// present — see MinSecretLength for the rules and why they are enforced here,
// on the input, rather than in the code that consumes them.
package config

import (
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Defaults mirrored from .env.example.
const (
	DefaultPort                   = 3001
	DefaultLogFormat              = "json"
	DefaultLocale                 = "en"
	DefaultCheckIntervalSeconds   = 60
	DefaultHeartbeatRetentionDays = 180
	// MinHeartbeatRetentionDays and MaxHeartbeatRetentionDays bound the runtime
	// heartbeat-retention setting (settings key "retention_days") that an admin
	// edits in Settings. The floor is a day because the policy is day-granular,
	// and a window at or below zero would mean "delete every heartbeat"; the
	// ceiling stops a fat-fingered value from pinning raw heartbeats effectively
	// forever. Env parsing of HEARTBEAT_RETENTION_DAYS only enforces the floor;
	// the ceiling is a guard on the runtime override, which is written straight to
	// the settings table and clamped again where the pruner reads it.
	MinHeartbeatRetentionDays = 1
	MaxHeartbeatRetentionDays = 3650 // ~10 years
	// DefaultDeliveryRetentionDays bounds the notification delivery log.
	//
	// A year rather than the heartbeats' six months, and deliberately so: this
	// is the record of who was told what and when, which is the thing an
	// operator reaches for after an incident and the thing an auditor asks
	// about. Raw heartbeats are samples; these are events, they arrive only on
	// a state change, and the table is small enough that keeping it longer
	// costs little.
	//
	// It also has a second reader now. Dispatcher.Restore asks the delivery log
	// which channels are still awaiting a recovery, so an unbounded table is no
	// longer only a storage question — it is work on the startup path of a
	// process whose job is to come back quickly.
	DefaultDeliveryRetentionDays = 365
	// DefaultTrashRetentionDays is how long a deleted monitor stays in the
	// recycle bin before it is destroyed for good, along with its heartbeats.
	//
	// Thirty days: long enough that a mistake is noticed — including the kind
	// noticed only when a report comes out wrong at the end of the month — and
	// short enough that the bin does not quietly become the reason the database
	// never shrinks. Zero disables the sweep and keeps the bin forever, which is
	// a deliberate choice rather than the default.
	DefaultTrashRetentionDays = 30
	DefaultWorkerPoolSize     = 0 // 0 = auto (4 x NumCPU)
	// DefaultSessionTTLDays is the session lifetime an unconfigured deployment
	// gets. An admin can override it at runtime in Settings, which takes
	// precedence over this value.
	DefaultSessionTTLDays = 30
	// MaxSessionTTLDays bounds SESSION_TTL_DAYS. An unbounded session cookie on
	// a shared machine defeats the point of having the setting.
	MaxSessionTTLDays = 365
	// Docker overrides this to /data/uploads (the declared volume); the relative
	// default resolves under systemd's WorkingDirectory=/var/lib/statwarden.
	DefaultUploadsDir = "./uploads"
	// Backup defaults. Docker overrides the directory to /data/backups, a
	// sibling of the uploads volume, so no new mount concept is introduced.
	//
	// Daily, seven kept, thirty days: a week of dumps survives the ordinary
	// "someone deleted the wrong thing on Friday" case without a restore
	// request, and the age rule bounds the volume. BackupKeep is what stops the
	// age rule from ever reaching zero.
	DefaultBackupDir           = "./backups"
	DefaultBackupIntervalHours = 24
	DefaultBackupKeep          = 7
	DefaultBackupMaxAgeDays    = 30

	// Response-time anomaly detection defaults. Detection is OFF by default;
	// these only take effect once ANOMALY_DETECTION_ENABLED=true. They are
	// deliberately conservative — a wide threshold, a long warmup and several
	// consecutive hits — so an enabled detector is slow to cry wolf.
	DefaultAnomalySensitivity = 3.5
	DefaultAnomalyMinSamples  = 30
	DefaultAnomalyConsecutive = 3

	// Flapping damping defaults. Five alertable state changes (to DOWN, or DOWN
	// back to UP) inside thirty minutes is two and a half full outages — well past
	// the point where a message per change still tells anyone something new. The
	// fifth change switches the monitor to one summary alert instead; it leaves
	// that mode once it has held one state for a whole window.
	DefaultFlapThreshold     = 5
	DefaultFlapWindowMinutes = 30

	// Check-interval backoff for a persistent outage. A host that has been down
	// for an hour is not going to be noticed back up any sooner for being asked
	// every minute; after DOWN_BACKOFF_AFTER_MINUTES it is checked every
	// DOWN_BACKOFF_INTERVAL_MINUTES instead (never more often than its own
	// interval), and the first successful check restores the normal cadence.
	DefaultDownBackoffAfterMinutes    = 60
	DefaultDownBackoffIntervalMinutes = 5
)

// Config is the fully parsed and validated application configuration.
type Config struct {
	// Warnings are non-fatal configuration complaints, logged once at startup
	// by the caller (Load has no logger of its own yet). They exist so a
	// weakness can be reported loudly WITHOUT refusing to boot — see
	// checkSecretStrength for the case that motivated them.
	Warnings []string

	// Required outside DEV_MODE.
	DatabaseURL   string
	SessionSecret string
	EncryptionKey string

	// Server.
	Port          int
	PublicBaseURL string
	// AllowedOrigins are extra browser origins (beyond PublicBaseURL) permitted
	// to open a WebSocket, for a deployment reached under more than one hostname.
	// Each is an absolute http(s) URL, explicitly configured and never derived
	// from a request header.
	AllowedOrigins []string
	LogLevel       slog.Level
	LogFormat      string // "json" or "text"
	// UploadsDir is the writable directory holding uploaded status-page logos.
	// It must not escape the working directory.
	UploadsDir string
	// BackupEnabled turns on scheduled pg_dump backups. Off by default: a job
	// that starts writing gigabytes to a volume is not something to switch on
	// for an operator without being asked, and many deployments already have
	// backups at the platform level (managed database, volume snapshots).
	BackupEnabled bool
	// BackupDir is where dumps are written. Like UploadsDir it must not escape
	// the working directory, and Docker points it at the mounted volume.
	BackupDir string
	// BackupIntervalHours is the time between dumps.
	BackupIntervalHours int
	// BackupKeep is how many of the newest dumps are retained no matter how old
	// they are. It is a floor rather than a target — see internal/backup.Prune
	// for why age alone would empty the directory exactly when it matters.
	BackupKeep int
	// BackupMaxAgeDays deletes dumps older than this, but never below
	// BackupKeep. Zero disables age-based pruning.
	BackupMaxAgeDays int
	// TrustedProxies lists the CIDRs of reverse proxies whose X-Forwarded-For
	// header may be believed when resolving the client IP. Empty (the default)
	// means the header is ignored entirely and the peer address is used
	// verbatim, so an unconfigured deployment cannot be spoofed.
	TrustedProxies []netip.Prefix

	// Behaviour defaults.
	DefaultLocale          string // "en" or "de"
	DefaultCheckInterval   time.Duration
	HeartbeatRetentionDays int
	// DeliveryRetentionDays bounds the notification delivery log; see
	// DefaultDeliveryRetentionDays.
	DeliveryRetentionDays int
	// TrashRetentionDays bounds the monitor recycle bin; see
	// DefaultTrashRetentionDays. 0 keeps binned monitors indefinitely.
	TrashRetentionDays int
	WorkerPoolSize     int // 0 = auto
	// SessionTTL is the DEFAULT session lifetime. The settings row
	// session_ttl_days overrides it at runtime when present and valid, so a
	// value set here that "will not take" is explained by that row.
	SessionTTL time.Duration

	// Single sign-on. Nil means SSO is disabled and no OIDC route is mounted.
	OIDC *OIDCConfig

	// AnomalyDetection configures response-time anomaly detection. It is
	// disabled by default so existing monitors are unaffected; when enabled a
	// monitor whose latency runs Sensitivity deviations above its rolling
	// baseline for Consecutive checks raises an event distinct from DOWN.
	AnomalyDetection AnomalyConfig

	// FlapThreshold is how many alertable state changes within FlapWindow put a
	// monitor into flapping mode, where its individual up/down alerts give way to
	// one summary. 0 disables flapping detection.
	FlapThreshold int
	FlapWindow    time.Duration

	// DownBackoffAfter is how long a monitor has to be DOWN before its checks
	// slow to DownBackoffInterval. 0 disables the backoff.
	DownBackoffAfter    time.Duration
	DownBackoffInterval time.Duration

	// SMTP is the transactional-mail delivery configuration. When it is not
	// Configured() the binary falls back to the log-only mailer and the affected
	// flows (password reset, subscriber verification, incident notifications)
	// behave honestly: they still run, but nothing is actually delivered.
	SMTP SMTPConfig

	// Development.
	DevMode bool
}

// SMTP encryption modes for the transactional mailer, matching the SMTP
// notification provider's vocabulary.
const (
	SMTPEncryptionNone     = "none"
	SMTPEncryptionStartTLS = "starttls"
	SMTPEncryptionTLS      = "tls" // implicit TLS ("SMTPS"), usually port 465
	// DefaultSMTPEncryption is STARTTLS: opportunistic-then-required TLS on the
	// submission port is the common default and the safe one.
	DefaultSMTPEncryption = SMTPEncryptionStartTLS
)

// AnomalyConfig holds the response-time anomaly-detection settings, parsed
// from the ANOMALY_* environment variables. Enabled is the switch: with it
// false (the default) the whole block is inert and the engine schedules no
// detector, so existing monitors behave exactly as before.
type AnomalyConfig struct {
	Enabled bool
	// Sensitivity is the number of standard deviations above baseline that
	// makes a check anomalous.
	Sensitivity float64
	// MinSamples is the warmup length before scoring begins.
	MinSamples int
	// Consecutive is the number of consecutive anomalous checks required
	// before a sustained anomaly is raised.
	Consecutive int
}

// SMTPConfig holds the transactional-mail delivery settings. It is parsed from
// the SMTP_* environment variables. Setting SMTP_HOST turns real delivery on;
// with it unset the whole block is inert and Configured() reports false.
type SMTPConfig struct {
	Host     string
	Port     int // 0 = derive from Encryption (465 for tls, else 587)
	Username string
	Password string
	From     string
	// Encryption is one of "none", "starttls" (default) or "tls".
	Encryption string
}

// Configured reports whether real SMTP delivery is switched on. The host is the
// single load-bearing knob: without a server to talk to there is nothing to do.
func (c SMTPConfig) Configured() bool { return c.Host != "" }

// EffectivePort resolves an unset (0) port to the conventional default for the
// selected encryption: 465 for implicit TLS, 587 for STARTTLS/none.
func (c SMTPConfig) EffectivePort() int {
	if c.Port != 0 {
		return c.Port
	}
	if strings.ToLower(c.Encryption) == SMTPEncryptionTLS {
		return 465
	}
	return 587
}

// OIDCConfig describes one configured identity provider.
type OIDCConfig struct {
	ProviderID   string
	DisplayName  string
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	// AutoProvision defaults to FALSE. When it is off, an assertion for a
	// subject nobody has linked is refused rather than turned into a new
	// account: an identity provider that can create accounts here is one that
	// can grant itself access, so enabling it must be a deliberate act.
	AutoProvision bool
	// DefaultRole is the role a provisioned account receives. Defaults to
	// viewer — the least privileged role — rather than to the writer role.
	DefaultRole string
	// AllowedEmailDomains optionally restricts provisioning by e-mail domain.
	// It is a provisioning filter only and is never used to resolve an existing
	// account from an asserted address.
	AllowedEmailDomains []string
	// AllowInsecureIssuer is set only when DEV_MODE permits a localhost http
	// issuer.
	AllowInsecureIssuer bool
}

// Load builds a Config from the given environment lookup function, normally
// os.LookupEnv. Set-but-empty (or whitespace-only) variables are treated as
// unset. All validation problems are collected and reported in one error.
func Load(lookup func(key string) (string, bool)) (*Config, error) {
	env := func(key string) (string, bool) {
		raw, ok := lookup(key)
		raw = strings.TrimSpace(raw)
		if !ok || raw == "" {
			return "", false
		}
		return raw, true
	}

	var problems []string
	addProblem := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	var warnings []string
	addWarning := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	intVar := func(key string, def, minimum int) int {
		raw, ok := env(key)
		if !ok {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < minimum {
			addProblem("%s: %q must be an integer >= %d", key, raw, minimum)
			return def
		}
		return n
	}

	enumVar := func(key, def string, allowed ...string) string {
		raw, ok := env(key)
		if !ok {
			return def
		}
		v := strings.ToLower(raw)
		for _, a := range allowed {
			if v == a {
				return v
			}
		}
		addProblem("%s: %q must be one of %s", key, raw, strings.Join(allowed, "|"))
		return def
	}

	cfg := &Config{}

	if raw, ok := env("DEV_MODE"); ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			addProblem("DEV_MODE: %q is not a boolean", raw)
		}
		cfg.DevMode = b
	}

	cfg.DatabaseURL, _ = env("DATABASE_URL")
	cfg.SessionSecret, _ = env("SESSION_SECRET")
	cfg.EncryptionKey, _ = env("ENCRYPTION_KEY")

	if !cfg.DevMode {
		var missing []string
		for _, req := range []struct{ key, val string }{
			{"DATABASE_URL", cfg.DatabaseURL},
			{"SESSION_SECRET", cfg.SessionSecret},
			{"ENCRYPTION_KEY", cfg.EncryptionKey},
		} {
			if req.val == "" {
				missing = append(missing, req.key)
			}
		}
		if len(missing) > 0 {
			addProblem("missing required environment variable(s): %s (set DEV_MODE=true to relax this for local development)",
				strings.Join(missing, ", "))
		}
		// Present is not the same as usable. Both of these values are consumed
		// verbatim — see MinSecretLength — so a copied example or a short
		// passphrase is a real weakness rather than a style problem. DEV_MODE
		// stays permissive: local work has nothing worth attacking, and `make
		// dev` must keep starting with no environment at all.
		checkSecretStrength("SESSION_SECRET", cfg.SessionSecret, addProblem, addWarning)
		checkSecretStrength("ENCRYPTION_KEY", cfg.EncryptionKey, addProblem, addWarning)
	}

	cfg.Port = DefaultPort
	if raw, ok := env("PORT"); ok {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			addProblem("PORT: %q must be an integer between 1 and 65535", raw)
		} else {
			cfg.Port = n
		}
	}

	switch enumVar("LOG_LEVEL", "info", "debug", "info", "warn", "error") {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		cfg.LogLevel = slog.LevelInfo
	}

	cfg.LogFormat = enumVar("LOG_FORMAT", DefaultLogFormat, "json", "text")
	cfg.DefaultLocale = enumVar("DEFAULT_LOCALE", DefaultLocale, "en", "de")

	cfg.PublicBaseURL = fmt.Sprintf("http://localhost:%d", cfg.Port)
	if raw, ok := env("PUBLIC_BASE_URL"); ok {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			addProblem("PUBLIC_BASE_URL: %q must be an absolute http(s) URL", raw)
		} else {
			cfg.PublicBaseURL = strings.TrimRight(raw, "/")
		}
	}

	if raw, ok := env("ALLOWED_ORIGINS"); ok {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			u, err := url.Parse(part)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				addProblem("ALLOWED_ORIGINS: %q must be an absolute http(s) URL", part)
				continue
			}
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, strings.TrimRight(part, "/"))
		}
	}

	cfg.UploadsDir = filepath.Clean(DefaultUploadsDir)
	if raw, ok := env("UPLOADS_DIR"); ok {
		clean := filepath.Clean(raw)
		// filepath.Clean already resolves interior ".." segments, so anything
		// left can only be a leading escape such as "../x" or "..".
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			addProblem("UPLOADS_DIR: %q must not escape the working directory", raw)
		} else {
			cfg.UploadsDir = clean
		}
	}

	cfg.BackupDir = filepath.Clean(DefaultBackupDir)
	if raw, ok := env("BACKUP_DIR"); ok {
		clean := filepath.Clean(raw)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			addProblem("BACKUP_DIR: %q must not escape the working directory", raw)
		} else {
			cfg.BackupDir = clean
		}
	}
	if raw, ok := env("BACKUP_ENABLED"); ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			addProblem("BACKUP_ENABLED: %q must be true or false", raw)
		} else {
			cfg.BackupEnabled = b
		}
	}
	cfg.BackupIntervalHours = intVar("BACKUP_INTERVAL_HOURS", DefaultBackupIntervalHours, 1)
	cfg.BackupKeep = intVar("BACKUP_KEEP", DefaultBackupKeep, 1)
	// Floor of 0, not 1: zero is the documented way to turn age-based pruning
	// off entirely and keep BACKUP_KEEP as the only rule.
	cfg.BackupMaxAgeDays = intVar("BACKUP_MAX_AGE_DAYS", DefaultBackupMaxAgeDays, 0)

	if raw, ok := env("TRUSTED_PROXIES"); ok {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			p, err := netip.ParsePrefix(part)
			if err != nil {
				addProblem("TRUSTED_PROXIES: %q must be a CIDR block (e.g. 10.0.0.0/8, 2001:db8::/32)", part)
				continue
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, p.Masked())
		}
	}

	cfg.DefaultCheckInterval = time.Duration(intVar("DEFAULT_CHECK_INTERVAL", DefaultCheckIntervalSeconds, 1)) * time.Second
	cfg.HeartbeatRetentionDays = intVar("HEARTBEAT_RETENTION_DAYS", DefaultHeartbeatRetentionDays, 1)
	cfg.DeliveryRetentionDays = intVar("DELIVERY_RETENTION_DAYS", DefaultDeliveryRetentionDays, 1)
	// Floor 0, not 1: zero is a meaningful setting here — "never empty the bin"
	// — whereas a zero-day heartbeat window would mean "delete everything".
	cfg.TrashRetentionDays = intVar("TRASH_RETENTION_DAYS", DefaultTrashRetentionDays, 0)
	cfg.WorkerPoolSize = intVar("WORKER_POOL_SIZE", DefaultWorkerPoolSize, 0)

	// intVar only enforces a floor, and an effectively permanent session is as
	// much of a problem as an instantly-expiring one, so the ceiling is checked
	// separately.
	sessionTTLDays := intVar("SESSION_TTL_DAYS", DefaultSessionTTLDays, 1)
	if sessionTTLDays > MaxSessionTTLDays {
		addProblem("SESSION_TTL_DAYS: %d must be at most %d", sessionTTLDays, MaxSessionTTLDays)
		sessionTTLDays = DefaultSessionTTLDays
	}
	cfg.SessionTTL = time.Duration(sessionTTLDays) * 24 * time.Hour

	cfg.AnomalyDetection = loadAnomaly(env, addProblem, intVar)

	// Floor 0 so that 0 can mean "off"; 1 is rejected separately, because a
	// threshold of one change would put every monitor into flapping mode on its
	// very first outage and swallow the one alert that matters most.
	cfg.FlapThreshold = intVar("FLAP_THRESHOLD", DefaultFlapThreshold, 0)
	if cfg.FlapThreshold == 1 {
		addProblem("FLAP_THRESHOLD: 1 would treat every single outage as flapping; use 0 (off) or >= 2")
		cfg.FlapThreshold = DefaultFlapThreshold
	}
	cfg.FlapWindow = time.Duration(intVar("FLAP_WINDOW_MINUTES", DefaultFlapWindowMinutes, 1)) * time.Minute
	cfg.DownBackoffAfter = time.Duration(intVar("DOWN_BACKOFF_AFTER_MINUTES", DefaultDownBackoffAfterMinutes, 0)) * time.Minute
	cfg.DownBackoffInterval = time.Duration(intVar("DOWN_BACKOFF_INTERVAL_MINUTES", DefaultDownBackoffIntervalMinutes, 1)) * time.Minute
	cfg.OIDC = loadOIDC(env, addProblem, enumVar, cfg.DevMode)
	cfg.SMTP = loadSMTP(env, addProblem, enumVar)

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	cfg.Warnings = warnings
	return cfg, nil
}

// MinSecretLength is the floor, in characters, for SESSION_SECRET and
// ENCRYPTION_KEY outside DEV_MODE.
//
// Neither value is stretched before use. ENCRYPTION_KEY is hashed exactly once
// with SHA-256 to produce the AES-256 key that seals monitor and notification
// credentials at rest (internal/notify/secrets), and SESSION_SECRET keys an
// HMAC directly. One hash is hundreds of millions of guesses per second on
// commodity hardware, so after a database leak nothing but the entropy of the
// configured string stands between the attacker and the plaintext.
//
// The obvious fix — a salted KDF — is not available: the derivation is baked
// into every row that is already encrypted, and changing it would make all of
// them permanently unreadable. That leaves the input as the only place a floor
// can be set, which is what this is.
//
// 32 is the floor because it is what a printable encoding of a properly random
// secret costs anyway: `openssl rand -base64 24` is exactly 32 characters and
// carries 192 bits, comfortably past the point where an offline dictionary or
// brute-force attack is worth starting. It is still short enough to paste into
// a .env file, and it is a length check and not an entropy measure — 32
// characters of prose remain a bad secret, which is what the two rules below
// are for.
const MinSecretLength = 32

// MinSecretDistinctChars catches the strings that clear MinSecretLength while
// carrying almost nothing: "aaaaaaaa…", "abababab…", a held-down keyboard row.
// Five is low enough that no real secret trips it — a 32-character string drawn
// from an alphabet of ten or more symbols misses that many distinct values only
// with vanishing probability — and high enough to reject every degenerate one.
const MinSecretDistinctChars = 5

// placeholderSecrets are fragments that mark a value nobody actually chose: the
// strings shipped in .env.example plus the usual stand-ins. Matching happens
// against the value lowercased with every non-alphanumeric character removed
// (see foldSecret), so "change-me", "CHANGE_ME" and "changeme" are one marker.
// The list is deliberately short and specific; a randomly generated secret
// containing one of these by accident is not a realistic outcome.
var placeholderSecrets = []string{
	"changeme",
	"changethis",
	"replaceme",
	"placeholder",
	"example",
	"insecure",
	"supersecret",
	"yourkey",
	"yoursecret",
	"randomchars",
}

// foldSecret lowercases s and drops everything that is not a letter or digit,
// so that punctuation style cannot hide a placeholder from placeholderSecrets.
func foldSecret(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// checkSecretStrength applies the MinSecretLength rules to one secret and
// reports every failure through addProblem, so a single startup attempt lists
// everything that is wrong with the environment. An empty value is left alone —
// it is already reported as missing, and a second complaint about its length
// would only be noise.
//
// No message ever echoes the value: these errors are printed to stdout and end
// up in the journal, and a secret that leaks into the logs of the process that
// rejected it would be its own bug. Lengths, counts and the matched placeholder
// word are enough to act on.
// A placeholder is FATAL; being merely weak is a WARNING.
//
// The asymmetry is deliberate and is about upgrades. Refusing to start on a
// short key would brick an existing instance on the release that introduces
// this check — and the operator could not fix it, because rotating
// ENCRYPTION_KEY makes everything already sealed under the old one unreadable.
// Bricking a monitoring system to improve the strength of a key that is already
// in use is the wrong trade: it takes down the thing that notices outages.
//
// A placeholder is different. Nobody has irreplaceable data under a value
// copied out of .env.example, and letting one boot is how an instance ends up
// encrypting production credentials under a key that is public knowledge. That
// one stays fatal, so `cp .env.example .env && docker compose up` still fails
// loudly on a NEW install, which is the case this check exists for.
func checkSecretStrength(key, value string, addProblem, addWarning func(string, ...any)) {
	if value == "" {
		return
	}

	weak := false
	runes := []rune(value)
	if len(runes) < MinSecretLength {
		weak = true
		addWarning("%s: %d characters is too short, use at least %d — it is consumed as-is "+
			"with no key-derivation function, so its entropy is all the protection there is "+
			"(generate one with: openssl rand -base64 48)", key, len(runes), MinSecretLength)
	}

	distinct := make(map[rune]struct{}, len(runes))
	for _, r := range runes {
		distinct[r] = struct{}{}
	}
	if len(distinct) < MinSecretDistinctChars {
		weak = true
		addWarning("%s: only %d distinct character(s), at least %d are recommended — padding a "+
			"secret to length by repetition adds no entropy (generate one with: "+
			"openssl rand -base64 48)", key, len(distinct), MinSecretDistinctChars)
	}

	folded := foldSecret(value)
	for _, p := range placeholderSecrets {
		if strings.Contains(folded, p) {
			weak = true
			addProblem("%s: contains %q and looks like a placeholder rather than a generated "+
				"secret (generate one with: openssl rand -base64 48)", key, p)
			break
		}
	}

	// Said once, and only when the operator is about to change this particular
	// variable: replacing ENCRYPTION_KEY is not free, and finding that out after
	// the restart is the expensive way to learn it.
	if weak && key == "ENCRYPTION_KEY" {
		addWarning("ENCRYPTION_KEY: note that anything already encrypted under the current key " +
			"cannot be read back under a new one — re-enter the affected monitor and " +
			"notification credentials after replacing it, and back the new key up with the " +
			"same care as the database")
	}
}

// loadSMTP parses the transactional-mail block. SMTP_HOST is the switch: with
// it unset the block is inert (Configured() == false) and the binary uses the
// log-only mailer. When a host IS given, SMTP_FROM becomes mandatory and must be
// a valid address, so a partially-configured relay fails startup loudly rather
// than silently dropping every reset and verification mail. Half-configuration
// the other way — credentials or a sender without a host — is rejected too,
// because it looks like mail was set up when in fact nothing will ever be sent.
func loadSMTP(
	env func(string) (string, bool),
	addProblem func(string, ...any),
	enumVar func(key, def string, allowed ...string) string,
) SMTPConfig {
	c := SMTPConfig{}
	c.Host, _ = env("SMTP_HOST")
	c.Username, _ = env("SMTP_USERNAME")
	c.Password, _ = env("SMTP_PASSWORD")
	c.From, _ = env("SMTP_FROM")
	c.Encryption = enumVar("SMTP_ENCRYPTION", DefaultSMTPEncryption,
		SMTPEncryptionNone, SMTPEncryptionStartTLS, SMTPEncryptionTLS)

	if raw, ok := env("SMTP_PORT"); ok {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			addProblem("SMTP_PORT: %q must be an integer between 1 and 65535", raw)
		} else {
			c.Port = n
		}
	}

	if c.Host != "" {
		switch c.From {
		case "":
			addProblem("SMTP_FROM is required when SMTP_HOST is set")
		default:
			if _, err := mail.ParseAddress(c.From); err != nil {
				addProblem("SMTP_FROM: %q is not a valid e-mail address", c.From)
			}
		}
	} else if c.From != "" || c.Username != "" || c.Password != "" {
		addProblem("SMTP_FROM/SMTP_USERNAME/SMTP_PASSWORD are set but SMTP_HOST is not; " +
			"set SMTP_HOST to enable mail delivery, or unset these to use the log-only mailer")
	}
	return c
}

// loadAnomaly parses the response-time anomaly-detection block. Detection is
// off unless ANOMALY_DETECTION_ENABLED=true; the sensitivity, warmup length and
// consecutive-hit count keep their conservative defaults unless overridden, so
// enabling the feature without tuning still behaves cautiously.
func loadAnomaly(
	env func(string) (string, bool),
	addProblem func(string, ...any),
	intVar func(key string, def, minimum int) int,
) AnomalyConfig {
	c := AnomalyConfig{
		Sensitivity: DefaultAnomalySensitivity,
		MinSamples:  DefaultAnomalyMinSamples,
		Consecutive: DefaultAnomalyConsecutive,
	}
	if raw, ok := env("ANOMALY_DETECTION_ENABLED"); ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			addProblem("ANOMALY_DETECTION_ENABLED: %q is not a boolean", raw)
		}
		c.Enabled = b
	}
	if raw, ok := env("ANOMALY_SENSITIVITY"); ok {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f <= 0 {
			addProblem("ANOMALY_SENSITIVITY: %q must be a number > 0", raw)
		} else {
			c.Sensitivity = f
		}
	}
	c.MinSamples = intVar("ANOMALY_MIN_SAMPLES", DefaultAnomalyMinSamples, 1)
	c.Consecutive = intVar("ANOMALY_CONSECUTIVE", DefaultAnomalyConsecutive, 1)
	return c
}

// Addr returns the listen address derived from Port, e.g. ":3001".
func (c *Config) Addr() string {
	return fmt.Sprintf(":%d", c.Port)
}

// MinAutoWorkerPoolSize floors the automatic sizing. Checks are I/O-bound and
// spend nearly all their time waiting on a socket, so the pool is sized for
// concurrency rather than for CPU — but on a one- or two-core host 4 x NumCPU
// is only 4 to 8 slots. A handful of monitors sitting in their (default 30s)
// timeout then occupy every slot, and everything else queues behind them,
// including a manual "check now" that consequently appears to do nothing for
// minutes. The floor keeps a small host usable; an operator who wants fewer
// still sets WORKER_POOL_SIZE explicitly.
const MinAutoWorkerPoolSize = 16

// EffectiveWorkerPoolSize resolves WORKER_POOL_SIZE=0 (auto) to 4 x NumCPU with
// a floor of MinAutoWorkerPoolSize, matching the sizing rationale in
// docs/architecture.md (checks are I/O-bound).
func (c *Config) EffectiveWorkerPoolSize() int {
	if c.WorkerPoolSize > 0 {
		return c.WorkerPoolSize
	}
	return max(4*runtime.NumCPU(), MinAutoWorkerPoolSize)
}

// loadOIDC parses the single-sign-on block. It returns nil when OIDC_ENABLED is
// not true, which is the default: an instance with no SSO configuration mounts
// no SSO routes at all rather than a set of endpoints that report 503.
func loadOIDC(
	env func(string) (string, bool),
	addProblem func(string, ...any),
	enumVar func(key, def string, allowed ...string) string,
	devMode bool,
) *OIDCConfig {
	raw, ok := env("OIDC_ENABLED")
	if !ok {
		return nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		addProblem("OIDC_ENABLED: %q is not a boolean", raw)
		return nil
	}
	if !enabled {
		return nil
	}

	c := &OIDCConfig{}
	c.ProviderID, _ = env("OIDC_PROVIDER_ID")
	if c.ProviderID == "" {
		c.ProviderID = "sso"
	}
	c.DisplayName, _ = env("OIDC_DISPLAY_NAME")
	if c.DisplayName == "" {
		c.DisplayName = "Single sign-on"
	}
	c.Issuer, _ = env("OIDC_ISSUER")
	c.ClientID, _ = env("OIDC_CLIENT_ID")
	c.ClientSecret, _ = env("OIDC_CLIENT_SECRET")

	// Each missing value is its own problem so one startup run reports them all.
	for _, req := range []struct{ key, val string }{
		{"OIDC_ISSUER", c.Issuer},
		{"OIDC_CLIENT_ID", c.ClientID},
		{"OIDC_CLIENT_SECRET", c.ClientSecret},
	} {
		if req.val == "" {
			addProblem("%s is required when OIDC_ENABLED=true", req.key)
		}
	}

	if c.Issuer != "" {
		u, err := url.Parse(c.Issuer)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
			addProblem("OIDC_ISSUER: %q must be an absolute http(s) URL", c.Issuer)
		case u.Scheme == "http":
			// The issuer pins the authority the whole flow trusts, and the token
			// exchange carries the client secret. Plain http is only tolerable on
			// a loopback host during development.
			host := u.Hostname()
			loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
			if devMode && loopback {
				c.AllowInsecureIssuer = true
			} else {
				addProblem("OIDC_ISSUER: %q must use https (plain http is permitted only on localhost with DEV_MODE=true)", c.Issuer)
			}
		}
		c.Issuer = strings.TrimRight(c.Issuer, "/")
	}

	if scopes, ok := env("OIDC_SCOPES"); ok {
		for _, part := range strings.Split(scopes, ",") {
			if part = strings.TrimSpace(part); part != "" {
				c.Scopes = append(c.Scopes, part)
			}
		}
	} else {
		c.Scopes = []string{"profile", "email"}
	}

	if raw, ok := env("OIDC_AUTO_PROVISION"); ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			addProblem("OIDC_AUTO_PROVISION: %q is not a boolean", raw)
		}
		c.AutoProvision = b
	}
	c.DefaultRole = enumVar("OIDC_DEFAULT_ROLE", "viewer", "viewer", "user", "admin")

	if domains, ok := env("OIDC_ALLOWED_EMAIL_DOMAINS"); ok {
		for _, part := range strings.Split(domains, ",") {
			if part = strings.TrimSpace(part); part != "" {
				c.AllowedEmailDomains = append(c.AllowedEmailDomains, strings.ToLower(part))
			}
		}
	}
	return c
}
