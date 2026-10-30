package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// lookupMap adapts a plain map to the os.LookupEnv signature.
func lookupMap(m map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// required returns the minimal environment accepted outside DEV_MODE.
//
// The two secrets look like generated ones because they have to: outside
// DEV_MODE Load enforces MinSecretLength and rejects repetitive or
// placeholder-looking values, so the previous "test-session-secret" no longer
// loads. Keep them at least MinSecretLength characters long, varied, and clear
// of every fragment in placeholderSecrets ("example" included).
func required() map[string]string {
	return map[string]string{
		"DATABASE_URL":   "postgres://statwarden:statwarden@localhost:5432/statwarden?sslmode=disable",
		"SESSION_SECRET": "Kx7Qv2mZs9Wd4Rt6Nv1Xc8Lp3Jg5Hf0Aq",
		"ENCRYPTION_KEY": "Zt4Nw8Rk1Vs6Mc3Xy9Bp2Ld7Qh5Gf0Jut",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.Port != 3001 {
		t.Errorf("Port = %d, want 3001", cfg.Port)
	}
	if cfg.PublicBaseURL != "http://localhost:3001" {
		t.Errorf("PublicBaseURL = %q, want http://localhost:3001", cfg.PublicBaseURL)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.DefaultLocale != "en" {
		t.Errorf("DefaultLocale = %q, want en", cfg.DefaultLocale)
	}
	if cfg.DefaultCheckInterval != 60*time.Second {
		t.Errorf("DefaultCheckInterval = %v, want 60s", cfg.DefaultCheckInterval)
	}
	if cfg.HeartbeatRetentionDays != 180 {
		t.Errorf("HeartbeatRetentionDays = %d, want 180", cfg.HeartbeatRetentionDays)
	}
	if cfg.DeliveryRetentionDays != 365 {
		t.Errorf("DeliveryRetentionDays = %d, want 365", cfg.DeliveryRetentionDays)
	}
	if cfg.TrashRetentionDays != 30 {
		t.Errorf("TrashRetentionDays = %d, want 30", cfg.TrashRetentionDays)
	}
	if cfg.WorkerPoolSize != 0 {
		t.Errorf("WorkerPoolSize = %d, want 0 (auto)", cfg.WorkerPoolSize)
	}
	if want := filepath.Clean(DefaultUploadsDir); cfg.UploadsDir != want {
		t.Errorf("UploadsDir = %q, want %q", cfg.UploadsDir, want)
	}
	if cfg.DevMode {
		t.Error("DevMode = true, want false by default")
	}
	// Backups are OFF unless asked for: the job writes to a volume, and many
	// deployments already back up at the platform level.
	if cfg.BackupEnabled {
		t.Error("BackupEnabled = true, want false by default")
	}
	if want := filepath.Clean(DefaultBackupDir); cfg.BackupDir != want {
		t.Errorf("BackupDir = %q, want %q", cfg.BackupDir, want)
	}
	if cfg.BackupIntervalHours != DefaultBackupIntervalHours {
		t.Errorf("BackupIntervalHours = %d, want %d", cfg.BackupIntervalHours, DefaultBackupIntervalHours)
	}
	if cfg.BackupKeep != DefaultBackupKeep {
		t.Errorf("BackupKeep = %d, want %d", cfg.BackupKeep, DefaultBackupKeep)
	}
	if cfg.BackupMaxAgeDays != DefaultBackupMaxAgeDays {
		t.Errorf("BackupMaxAgeDays = %d, want %d", cfg.BackupMaxAgeDays, DefaultBackupMaxAgeDays)
	}
	if cfg.FlapThreshold != DefaultFlapThreshold {
		t.Errorf("FlapThreshold = %d, want %d", cfg.FlapThreshold, DefaultFlapThreshold)
	}
	if cfg.FlapWindow != 30*time.Minute {
		t.Errorf("FlapWindow = %v, want 30m", cfg.FlapWindow)
	}
	if cfg.DownBackoffAfter != time.Hour {
		t.Errorf("DownBackoffAfter = %v, want 1h", cfg.DownBackoffAfter)
	}
	if cfg.DownBackoffInterval != 5*time.Minute {
		t.Errorf("DownBackoffInterval = %v, want 5m", cfg.DownBackoffInterval)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	_, err := Load(lookupMap(nil))
	if err == nil {
		t.Fatal("Load() with empty environment succeeded, want error")
	}
	for _, name := range []string{"DATABASE_URL", "SESSION_SECRET", "ENCRYPTION_KEY"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention missing variable %s", err, name)
		}
	}
}

func TestLoadMissingSingleRequired(t *testing.T) {
	env := required()
	delete(env, "ENCRYPTION_KEY")
	_, err := Load(lookupMap(env))
	if err == nil {
		t.Fatal("Load() without ENCRYPTION_KEY succeeded, want error")
	}
	if !strings.Contains(err.Error(), "ENCRYPTION_KEY") {
		t.Errorf("error %q does not mention ENCRYPTION_KEY", err)
	}
	if strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q mentions DATABASE_URL although it is set", err)
	}
}

func TestLoadEmptyStringCountsAsUnset(t *testing.T) {
	env := required()
	env["SESSION_SECRET"] = "   "
	_, err := Load(lookupMap(env))
	if err == nil {
		t.Fatal("Load() with whitespace-only SESSION_SECRET succeeded, want error")
	}
}

func TestDevModeRelaxesRequiredVars(t *testing.T) {
	cfg, err := Load(lookupMap(map[string]string{"DEV_MODE": "true"}))
	if err != nil {
		t.Fatalf("Load() in DEV_MODE without secrets errored: %v", err)
	}
	if !cfg.DevMode {
		t.Error("DevMode = false, want true")
	}
	if cfg.DatabaseURL != "" {
		t.Errorf("DatabaseURL = %q, want empty", cfg.DatabaseURL)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := required()
	env["PORT"] = "8080"
	env["PUBLIC_BASE_URL"] = "https://status.example.com/"
	env["LOG_LEVEL"] = "DEBUG" // case-insensitive
	env["LOG_FORMAT"] = "text"
	env["DEFAULT_LOCALE"] = "de"
	env["DEFAULT_CHECK_INTERVAL"] = "30"
	env["HEARTBEAT_RETENTION_DAYS"] = "90"
	env["DELIVERY_RETENTION_DAYS"] = "30"
	env["TRASH_RETENTION_DAYS"] = "7"
	env["BACKUP_ENABLED"] = "true"
	env["BACKUP_DIR"] = "/data/backups"
	env["BACKUP_INTERVAL_HOURS"] = "6"
	env["BACKUP_KEEP"] = "3"
	// 0 is meaningful: it turns age pruning off and leaves BACKUP_KEEP alone.
	env["BACKUP_MAX_AGE_DAYS"] = "0"
	env["WORKER_POOL_SIZE"] = "16"
	// 0 is meaningful for both: it switches the feature off.
	env["FLAP_THRESHOLD"] = "0"
	env["FLAP_WINDOW_MINUTES"] = "10"
	env["DOWN_BACKOFF_AFTER_MINUTES"] = "0"
	env["DOWN_BACKOFF_INTERVAL_MINUTES"] = "15"
	env["UPLOADS_DIR"] = "/data/uploads/" // cleaned
	// Sloppy spacing, a trailing slash and an empty entry that parsing normalises.
	env["ALLOWED_ORIGINS"] = "https://nas.example.com, http://statwarden.local:3001/ ,"

	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if want := []string{"https://nas.example.com", "http://statwarden.local:3001"}; !slices.Equal(cfg.AllowedOrigins, want) {
		t.Errorf("AllowedOrigins = %v, want %v", cfg.AllowedOrigins, want)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.PublicBaseURL != "https://status.example.com" {
		t.Errorf("PublicBaseURL = %q, want trailing slash trimmed", cfg.PublicBaseURL)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", cfg.LogFormat)
	}
	if cfg.DefaultLocale != "de" {
		t.Errorf("DefaultLocale = %q, want de", cfg.DefaultLocale)
	}
	if cfg.DefaultCheckInterval != 30*time.Second {
		t.Errorf("DefaultCheckInterval = %v, want 30s", cfg.DefaultCheckInterval)
	}
	if cfg.HeartbeatRetentionDays != 90 {
		t.Errorf("HeartbeatRetentionDays = %d, want 90", cfg.HeartbeatRetentionDays)
	}
	if cfg.DeliveryRetentionDays != 30 {
		t.Errorf("DeliveryRetentionDays = %d, want 30", cfg.DeliveryRetentionDays)
	}
	if cfg.TrashRetentionDays != 7 {
		t.Errorf("TrashRetentionDays = %d, want 7", cfg.TrashRetentionDays)
	}
	if !cfg.BackupEnabled {
		t.Error("BackupEnabled = false, want true")
	}
	if cfg.BackupDir != filepath.Clean("/data/backups") {
		t.Errorf("BackupDir = %q, want /data/backups", cfg.BackupDir)
	}
	if cfg.BackupIntervalHours != 6 {
		t.Errorf("BackupIntervalHours = %d, want 6", cfg.BackupIntervalHours)
	}
	if cfg.BackupKeep != 3 {
		t.Errorf("BackupKeep = %d, want 3", cfg.BackupKeep)
	}
	if cfg.BackupMaxAgeDays != 0 {
		t.Errorf("BackupMaxAgeDays = %d, want 0 (age pruning off)", cfg.BackupMaxAgeDays)
	}
	if cfg.WorkerPoolSize != 16 {
		t.Errorf("WorkerPoolSize = %d, want 16", cfg.WorkerPoolSize)
	}
	if cfg.FlapThreshold != 0 {
		t.Errorf("FlapThreshold = %d, want 0 (off)", cfg.FlapThreshold)
	}
	if cfg.FlapWindow != 10*time.Minute {
		t.Errorf("FlapWindow = %v, want 10m", cfg.FlapWindow)
	}
	if cfg.DownBackoffAfter != 0 {
		t.Errorf("DownBackoffAfter = %v, want 0 (off)", cfg.DownBackoffAfter)
	}
	if cfg.DownBackoffInterval != 15*time.Minute {
		t.Errorf("DownBackoffInterval = %v, want 15m", cfg.DownBackoffInterval)
	}
	if want := filepath.Clean("/data/uploads"); cfg.UploadsDir != want {
		t.Errorf("UploadsDir = %q, want %q", cfg.UploadsDir, want)
	}
}

// TestLoadTrustedProxiesDefaultEmpty pins the safe default: without the knob no
// proxy is trusted, so X-Forwarded-For is ignored entirely.
func TestLoadTrustedProxiesDefaultEmpty(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want empty by default", cfg.TrustedProxies)
	}
}

func TestLoadTrustedProxies(t *testing.T) {
	env := required()
	// Mixed families, sloppy spacing, an empty entry and a host-bit set that
	// masking must normalise away.
	env["TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.7/16 ,, 2001:db8::/32"

	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	want := []string{"10.0.0.0/8", "192.168.0.0/16", "2001:db8::/32"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %d entries", cfg.TrustedProxies, len(want))
	}
	for i, w := range want {
		if got := cfg.TrustedProxies[i].String(); got != w {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, got, w)
		}
	}
}

// TestLoadAllowedOriginsDefaultEmpty pins that without the knob no extra origin
// is trusted, so the WebSocket check falls back to PUBLIC_BASE_URL alone.
func TestLoadAllowedOriginsDefaultEmpty(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(cfg.AllowedOrigins) != 0 {
		t.Errorf("AllowedOrigins = %v, want empty by default", cfg.AllowedOrigins)
	}
}

func TestLoadInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"port not a number", "PORT", "http"},
		{"port out of range", "PORT", "70000"},
		{"port zero", "PORT", "0"},
		{"bad log level", "LOG_LEVEL", "verbose"},
		{"bad log format", "LOG_FORMAT", "xml"},
		{"bad locale", "DEFAULT_LOCALE", "fr"},
		{"bad check interval", "DEFAULT_CHECK_INTERVAL", "0"},
		{"bad retention", "HEARTBEAT_RETENTION_DAYS", "-1"},
		{"bad delivery retention", "DELIVERY_RETENTION_DAYS", "0"},
		// -1 and not 0: zero is a VALID trash window ("never empty the bin"),
		// which is why this knob's floor is 0 and not 1.
		{"bad trash retention", "TRASH_RETENTION_DAYS", "-1"},
		{"bad backup enabled", "BACKUP_ENABLED", "sometimes"},
		{"bad backup interval", "BACKUP_INTERVAL_HOURS", "0"},
		{"bad backup keep", "BACKUP_KEEP", "0"},
		{"bad backup max age", "BACKUP_MAX_AGE_DAYS", "-1"},
		{"escaping backup dir", "BACKUP_DIR", "../elsewhere"},
		{"bad worker pool", "WORKER_POOL_SIZE", "-2"},
		{"negative flap threshold", "FLAP_THRESHOLD", "-1"},
		// 1 is not a threshold, it is "every outage is flapping".
		{"flap threshold of one", "FLAP_THRESHOLD", "1"},
		{"bad flap window", "FLAP_WINDOW_MINUTES", "0"},
		{"bad backoff after", "DOWN_BACKOFF_AFTER_MINUTES", "-5"},
		{"bad backoff interval", "DOWN_BACKOFF_INTERVAL_MINUTES", "0"},
		{"bad dev mode", "DEV_MODE", "yes-please"},
		{"bad base url", "PUBLIC_BASE_URL", "not a url"},
		{"base url without scheme", "PUBLIC_BASE_URL", "status.example.com"},
		// filepath.Clean cannot collapse a leading escape, which is the only
		// thing worth rejecting: "/data/../etc" legitimately cleans to "/etc".
		{"uploads dir escapes upwards", "UPLOADS_DIR", "../../etc"},
		{"trusted proxy without a mask", "TRUSTED_PROXIES", "10.0.0.1"},
		{"trusted proxy not an address", "TRUSTED_PROXIES", "proxy.example.com/32"},
		{"trusted proxy with a bad mask", "TRUSTED_PROXIES", "10.0.0.0/99"},
		{"trusted proxy invalid among valid", "TRUSTED_PROXIES", "10.0.0.0/8, nonsense"},
		{"bad allowed origin", "ALLOWED_ORIGINS", "not a url"},
		{"allowed origin without scheme", "ALLOWED_ORIGINS", "nas.example.com"},
		{"allowed origin invalid among valid", "ALLOWED_ORIGINS", "https://ok.example, nonsense"},
		// A placeholder secret is fatal; merely WEAK is a warning, so the two
		// short values that used to sit here live in the strength tests below.
		{"placeholder encryption key", "ENCRYPTION_KEY", "change-me-32-byte-key-for-secrets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := required()
			env[tc.key] = tc.val
			_, err := Load(lookupMap(env))
			if err == nil {
				t.Fatalf("Load() with %s=%q succeeded, want error", tc.key, tc.val)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not mention %s", err, tc.key)
			}
		})
	}
}

func TestLoadCollectsAllProblems(t *testing.T) {
	env := map[string]string{"PORT": "abc", "LOG_FORMAT": "xml"}
	_, err := Load(lookupMap(env))
	if err == nil {
		t.Fatal("Load() succeeded, want error")
	}
	for _, want := range []string{"PORT", "LOG_FORMAT", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// --- secret strength ----------------------------------------------------------
//
// SESSION_SECRET and ENCRYPTION_KEY are used exactly as configured: one SHA-256
// to the AES key, one HMAC key. The derivation cannot be strengthened without
// orphaning every already-encrypted row, so the input is where the requirement
// has to live, and these tests are what keeps it there.

// TestSecretStrengthRejectsPlaceholders pins the FATAL half of the rule: a
// value copied out of .env.example or a template must never boot, because it is
// public knowledge and nobody has irreplaceable data sealed under it yet.
func TestSecretStrengthRejectsPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		// The values .env.example used to ship, and the shapes they mutate into.
		{"old example encryption key", "ENCRYPTION_KEY", "change-me-32-byte-key-for-notification-secrets"},
		{"old example session secret", "SESSION_SECRET", "change-me-64-random-chars-change-me-64-random-chars"},
		{"placeholder with other separators", "SESSION_SECRET", "CHANGE_ME_Kx7Qv2mZs9Wd4Rt6Nv1Xc8"},
		{"placeholder without separators", "ENCRYPTION_KEY", "changemeKx7Qv2mZs9Wd4Rt6Nv1Xc8Lp3"},
		{"documentation sample", "ENCRYPTION_KEY", "statwarden-example-key-do-not-use-anywhere"},
		{"unfilled template", "SESSION_SECRET", "put-your-secret-here-before-starting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := required()
			env[tc.key] = tc.val
			_, err := Load(lookupMap(env))
			if err == nil {
				t.Fatalf("Load() with a weak %s succeeded; a value that is hashed once into the "+
					"AES key must not be accepted just because it is non-empty", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}

// TestSecretStrengthAcceptsGeneratedValues pins the other side: a real secret
// must load, including one sitting exactly on the floor. A rule that also
// rejects `openssl rand -base64 24` output would only teach operators to set
// DEV_MODE=true in production.
func TestSecretStrengthAcceptsGeneratedValues(t *testing.T) {
	atFloor := strings.Repeat("aB3$z7Kq", 4) // exactly MinSecretLength characters
	if len([]rune(atFloor)) != MinSecretLength {
		t.Fatalf("test fixture is %d characters, want exactly %d", len([]rune(atFloor)), MinSecretLength)
	}
	for _, key := range []string{"SESSION_SECRET", "ENCRYPTION_KEY"} {
		for _, val := range []string{atFloor, "hK3+pQ9zR1sV6wX2yB8nC5mD7fG0jL4tA6uE2iO9k="} {
			env := required()
			env[key] = val
			if _, err := Load(lookupMap(env)); err != nil {
				t.Errorf("Load() rejected a generated %s: %v", key, err)
			}
		}
	}
}

// TestSecretStrengthErrorDoesNotEchoTheSecret pins that the rejection message
// stays printable. It goes to stdout and into the journal, so a secret quoted
// back in it would be leaked by the very check meant to protect it.
func TestSecretStrengthErrorDoesNotEchoTheSecret(t *testing.T) {
	const weak = "changeme-Kx7Qv2mZs9Wd4Rt6Nv1Xc8Lp3"
	env := required()
	env["ENCRYPTION_KEY"] = weak
	_, err := Load(lookupMap(env))
	if err == nil {
		t.Fatal("Load() accepted a placeholder ENCRYPTION_KEY")
	}
	if strings.Contains(err.Error(), weak) {
		t.Errorf("the error echoes the configured secret: %q", err)
	}
}

// TestDevModeAllowsWeakSecrets pins that local development stays frictionless:
// `make dev` runs with no environment at all, and a throwaway key in a
// docker-compose.dev.yml stack must not be a startup failure.
func TestDevModeAllowsWeakSecrets(t *testing.T) {
	env := map[string]string{
		"DEV_MODE":       "true",
		"SESSION_SECRET": "dev",
		"ENCRYPTION_KEY": "changeme",
	}
	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() in DEV_MODE with weak secrets errored: %v", err)
	}
	if cfg.EncryptionKey != "changeme" {
		t.Errorf("EncryptionKey = %q, want the value passed through untouched", cfg.EncryptionKey)
	}
}

// TestEnvExampleSecretsAreRejected reads the file operators actually copy and
// asserts the shipped values do not start an instance. A copyable example is
// how a "random" key ends up identical on every deployment in the world.
func TestEnvExampleSecretsAreRejected(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("reading .env.example: %v", err)
	}
	for _, key := range []string{"SESSION_SECRET", "ENCRYPTION_KEY"} {
		val, ok := envExampleValue(string(raw), key)
		if !ok {
			t.Errorf(".env.example does not define %s; every variable belongs there", key)
			continue
		}
		env := required()
		env[key] = val
		if _, err := Load(lookupMap(env)); err == nil {
			t.Errorf(".env.example ships %s=%q and Load accepts it — the shipped value must be "+
				"an obvious placeholder, not something that works when copied", key, val)
		}
	}
}

// envExampleValue extracts an uncommented KEY=value line from a dotenv file.
func envExampleValue(content, key string) (string, bool) {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if after, found := strings.CutPrefix(line, key+"="); found {
			return strings.TrimSpace(after), true
		}
	}
	return "", false
}

func TestEffectiveWorkerPoolSize(t *testing.T) {
	auto := (&Config{WorkerPoolSize: 0}).EffectiveWorkerPoolSize()
	want := max(4*runtime.NumCPU(), MinAutoWorkerPoolSize)
	if auto != want {
		t.Errorf("EffectiveWorkerPoolSize() = %d, want %d (4 x NumCPU, floored)", auto, want)
	}
	// The floor is what keeps a single-core host from starving its own worker
	// pool, so assert it directly rather than only through the expression above.
	if auto < MinAutoWorkerPoolSize {
		t.Errorf("EffectiveWorkerPoolSize() = %d, want at least the %d floor", auto, MinAutoWorkerPoolSize)
	}
	if got := (&Config{WorkerPoolSize: 7}).EffectiveWorkerPoolSize(); got != 7 {
		t.Errorf("EffectiveWorkerPoolSize() = %d, want explicit 7", got)
	}
}

func TestAddr(t *testing.T) {
	if got := (&Config{Port: 3999}).Addr(); got != ":3999" {
		t.Errorf("Addr() = %q, want :3999", got)
	}
}

// --- session lifetime ---------------------------------------------------------

func TestSessionTTLDefault(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := time.Duration(DefaultSessionTTLDays) * 24 * time.Hour; cfg.SessionTTL != want {
		t.Errorf("SessionTTL = %s, want %s", cfg.SessionTTL, want)
	}
}

func TestSessionTTLOverride(t *testing.T) {
	env := required()
	env["SESSION_TTL_DAYS"] = "7"
	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := 7 * 24 * time.Hour; cfg.SessionTTL != want {
		t.Errorf("SessionTTL = %s, want %s", cfg.SessionTTL, want)
	}
}

// TestSessionTTLRejectsOutOfRange pins both bounds. A value at or below zero
// would mint already-expired sessions and lock every user out; an unbounded one
// is effectively a permanent cookie and defeats the point of the setting.
func TestSessionTTLRejectsOutOfRange(t *testing.T) {
	for _, bad := range []string{"0", "-1", "400", "abc", "1.5"} {
		env := required()
		env["SESSION_TTL_DAYS"] = bad
		_, err := Load(lookupMap(env))
		if err == nil {
			t.Errorf("SESSION_TTL_DAYS=%q was accepted, want a startup error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "SESSION_TTL_DAYS") {
			t.Errorf("SESSION_TTL_DAYS=%q error does not name the variable: %v", bad, err)
		}
	}
}

// --- response-time anomaly detection ------------------------------------------

// TestAnomalyDisabledByDefault pins the safe default: with no ANOMALY_* set the
// feature is off and the conservative defaults are still resolved, so a later
// ANOMALY_DETECTION_ENABLED=true alone behaves cautiously.
func TestAnomalyDisabledByDefault(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	a := cfg.AnomalyDetection
	if a.Enabled {
		t.Error("AnomalyDetection.Enabled = true, want false by default")
	}
	if a.Sensitivity != DefaultAnomalySensitivity {
		t.Errorf("Sensitivity = %v, want %v", a.Sensitivity, DefaultAnomalySensitivity)
	}
	if a.MinSamples != DefaultAnomalyMinSamples {
		t.Errorf("MinSamples = %d, want %d", a.MinSamples, DefaultAnomalyMinSamples)
	}
	if a.Consecutive != DefaultAnomalyConsecutive {
		t.Errorf("Consecutive = %d, want %d", a.Consecutive, DefaultAnomalyConsecutive)
	}
}

func TestAnomalyOverrides(t *testing.T) {
	env := required()
	env["ANOMALY_DETECTION_ENABLED"] = "true"
	env["ANOMALY_SENSITIVITY"] = "2.5"
	env["ANOMALY_MIN_SAMPLES"] = "50"
	env["ANOMALY_CONSECUTIVE"] = "5"

	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	a := cfg.AnomalyDetection
	if !a.Enabled {
		t.Error("Enabled = false, want true")
	}
	if a.Sensitivity != 2.5 {
		t.Errorf("Sensitivity = %v, want 2.5", a.Sensitivity)
	}
	if a.MinSamples != 50 {
		t.Errorf("MinSamples = %d, want 50", a.MinSamples)
	}
	if a.Consecutive != 5 {
		t.Errorf("Consecutive = %d, want 5", a.Consecutive)
	}
}

func TestAnomalyInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"enabled not a boolean", "ANOMALY_DETECTION_ENABLED", "sometimes"},
		{"sensitivity not a number", "ANOMALY_SENSITIVITY", "loose"},
		{"sensitivity zero", "ANOMALY_SENSITIVITY", "0"},
		{"sensitivity negative", "ANOMALY_SENSITIVITY", "-1.5"},
		{"min samples below floor", "ANOMALY_MIN_SAMPLES", "0"},
		{"consecutive below floor", "ANOMALY_CONSECUTIVE", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := required()
			env[tc.key] = tc.val
			_, err := Load(lookupMap(env))
			if err == nil {
				t.Fatalf("Load() with %s=%q succeeded, want error", tc.key, tc.val)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not mention %s", err, tc.key)
			}
		})
	}
}

// --- single sign-on -----------------------------------------------------------

// TestOIDCDisabledByDefault pins that an instance with no SSO variables gets no
// SSO at all, rather than a half-configured provider.
func TestOIDCDisabledByDefault(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OIDC != nil {
		t.Errorf("OIDC = %+v, want nil when OIDC_ENABLED is unset", cfg.OIDC)
	}
}

func oidcEnv() map[string]string {
	env := required()
	env["OIDC_ENABLED"] = "true"
	env["OIDC_ISSUER"] = "https://idp.example/realms/statwarden"
	env["OIDC_CLIENT_ID"] = "statwarden"
	env["OIDC_CLIENT_SECRET"] = "shh"
	return env
}

// TestOIDCAutoProvisionDefaultsOff is the config-level half of S2. An identity
// provider that can create accounts here is one that can grant itself access, so
// enabling it must be a deliberate act rather than a default.
func TestOIDCAutoProvisionDefaultsOff(t *testing.T) {
	cfg, err := Load(lookupMap(oidcEnv()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OIDC == nil {
		t.Fatal("OIDC = nil, want a configured provider")
	}
	if cfg.OIDC.AutoProvision {
		t.Error("AutoProvision defaults to true; it must default to FALSE — an IdP that can " +
			"mint local accounts can grant itself access")
	}
	if cfg.OIDC.DefaultRole != "viewer" {
		t.Errorf("DefaultRole = %q, want viewer (least privilege)", cfg.OIDC.DefaultRole)
	}
}

func TestOIDCAutoProvisionOptIn(t *testing.T) {
	env := oidcEnv()
	env["OIDC_AUTO_PROVISION"] = "true"
	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.OIDC.AutoProvision {
		t.Error("OIDC_AUTO_PROVISION=true was not honoured")
	}
}

// TestOIDCRequiresCredentials pins that a half-configured provider fails startup
// loudly rather than mounting routes that cannot work.
func TestOIDCRequiresCredentials(t *testing.T) {
	for _, missing := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET"} {
		env := oidcEnv()
		delete(env, missing)
		_, err := Load(lookupMap(env))
		if err == nil {
			t.Errorf("missing %s was accepted, want a startup error", missing)
			continue
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the error for missing %s does not name it: %v", missing, err)
		}
	}
}

// TestOIDCIssuerMustBeHTTPS pins that the pinned authority — and the channel the
// client secret travels over — cannot be plain http outside local development.
func TestOIDCIssuerMustBeHTTPS(t *testing.T) {
	env := oidcEnv()
	env["OIDC_ISSUER"] = "http://idp.example"
	if _, err := Load(lookupMap(env)); err == nil {
		t.Error("a plain-http issuer was accepted outside DEV_MODE")
	}

	// The escape hatch is DEV_MODE plus loopback, and nothing wider.
	env = oidcEnv()
	env["OIDC_ISSUER"] = "http://localhost:8080/realms/statwarden"
	env["DEV_MODE"] = "true"
	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("http on localhost under DEV_MODE = %v, want accepted", err)
	}
	if !cfg.OIDC.AllowInsecureIssuer {
		t.Error("AllowInsecureIssuer was not set for the DEV_MODE localhost case")
	}

	// DEV_MODE alone must not license a remote plaintext issuer.
	env = oidcEnv()
	env["OIDC_ISSUER"] = "http://idp.example"
	env["DEV_MODE"] = "true"
	if _, err := Load(lookupMap(env)); err == nil {
		t.Error("DEV_MODE allowed a plain-http issuer on a REMOTE host; the exemption is for " +
			"loopback only")
	}
}

func TestOIDCIssuerTrailingSlashTrimmed(t *testing.T) {
	env := oidcEnv()
	env["OIDC_ISSUER"] = "https://idp.example/realms/statwarden/"
	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.OIDC.Issuer, "https://idp.example/realms/statwarden"; got != want {
		t.Errorf("Issuer = %q, want %q — the issuer is compared byte for byte against the "+
			"discovery document and every token, so it must be normalised once here", got, want)
	}
}

func TestOIDCDefaultRoleIsValidated(t *testing.T) {
	env := oidcEnv()
	env["OIDC_DEFAULT_ROLE"] = "superuser"
	if _, err := Load(lookupMap(env)); err == nil {
		t.Error("an unknown OIDC_DEFAULT_ROLE was accepted")
	}
}

// --- transactional mail (SMTP) ------------------------------------------------

// TestSMTPDisabledByDefault pins the honest fallback: with no SMTP_* variables
// the block is inert, so main.go uses the log-only mailer and the operator is
// warned rather than mail being silently dropped as if it had been sent.
func TestSMTPDisabledByDefault(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SMTP.Configured() {
		t.Errorf("SMTP.Configured() = true with no SMTP_* set, want false")
	}
	// The default encryption is still resolved so a later SMTP_HOST alone works.
	if cfg.SMTP.Encryption != DefaultSMTPEncryption {
		t.Errorf("SMTP.Encryption = %q, want %q", cfg.SMTP.Encryption, DefaultSMTPEncryption)
	}
}

func TestSMTPFullConfig(t *testing.T) {
	env := required()
	env["SMTP_HOST"] = "smtp.example.com"
	env["SMTP_PORT"] = "2525"
	env["SMTP_USERNAME"] = "mailer@example.com"
	env["SMTP_PASSWORD"] = "s3cret"
	env["SMTP_FROM"] = "Statwarden <status@example.com>"
	env["SMTP_ENCRYPTION"] = "TLS" // case-insensitive

	cfg, err := Load(lookupMap(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if !cfg.SMTP.Configured() {
		t.Fatal("SMTP.Configured() = false, want true")
	}
	if cfg.SMTP.Host != "smtp.example.com" {
		t.Errorf("Host = %q, want smtp.example.com", cfg.SMTP.Host)
	}
	if cfg.SMTP.Port != 2525 || cfg.SMTP.EffectivePort() != 2525 {
		t.Errorf("Port = %d / EffectivePort = %d, want 2525", cfg.SMTP.Port, cfg.SMTP.EffectivePort())
	}
	if cfg.SMTP.Username != "mailer@example.com" || cfg.SMTP.Password != "s3cret" {
		t.Errorf("credentials not parsed: %+v", cfg.SMTP)
	}
	if cfg.SMTP.From != "Statwarden <status@example.com>" {
		t.Errorf("From = %q, want the RFC 5322 name-addr", cfg.SMTP.From)
	}
	if cfg.SMTP.Encryption != SMTPEncryptionTLS {
		t.Errorf("Encryption = %q, want tls", cfg.SMTP.Encryption)
	}
}

// TestSMTPEffectivePortDefaults pins the conventional port derivation when
// SMTP_PORT is omitted: implicit TLS gets 465, everything else the submission
// port 587.
func TestSMTPEffectivePortDefaults(t *testing.T) {
	for _, tc := range []struct {
		enc  string
		want int
	}{
		{SMTPEncryptionTLS, 465},
		{SMTPEncryptionStartTLS, 587},
		{SMTPEncryptionNone, 587},
	} {
		env := required()
		env["SMTP_HOST"] = "smtp.example.com"
		env["SMTP_FROM"] = "status@example.com"
		env["SMTP_ENCRYPTION"] = tc.enc
		cfg, err := Load(lookupMap(env))
		if err != nil {
			t.Fatalf("Load(%s) error = %v", tc.enc, err)
		}
		if got := cfg.SMTP.EffectivePort(); got != tc.want {
			t.Errorf("EffectivePort(%s) = %d, want %d", tc.enc, got, tc.want)
		}
	}
}

func TestSMTPRequiresFromWhenHostSet(t *testing.T) {
	env := required()
	env["SMTP_HOST"] = "smtp.example.com"
	// No SMTP_FROM.
	_, err := Load(lookupMap(env))
	if err == nil {
		t.Fatal("SMTP_HOST without SMTP_FROM was accepted, want a startup error")
	}
	if !strings.Contains(err.Error(), "SMTP_FROM") {
		t.Errorf("error %q does not name SMTP_FROM", err)
	}
}

func TestSMTPInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "bad port",
			env:  map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "s@example.com", "SMTP_PORT": "70000"},
			want: "SMTP_PORT",
		},
		{
			name: "port not a number",
			env:  map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "s@example.com", "SMTP_PORT": "smtp"},
			want: "SMTP_PORT",
		},
		{
			name: "bad encryption",
			env:  map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "s@example.com", "SMTP_ENCRYPTION": "ssl"},
			want: "SMTP_ENCRYPTION",
		},
		{
			name: "invalid from",
			env:  map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "not-an-address"},
			want: "SMTP_FROM",
		},
		{
			name: "from without host",
			env:  map[string]string{"SMTP_FROM": "s@example.com"},
			want: "SMTP_HOST",
		},
		{
			name: "credentials without host",
			env:  map[string]string{"SMTP_USERNAME": "u", "SMTP_PASSWORD": "p"},
			want: "SMTP_HOST",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := required()
			for k, v := range tc.env {
				env[k] = v
			}
			_, err := Load(lookupMap(env))
			if err == nil {
				t.Fatalf("Load() with %v succeeded, want error", tc.env)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %s", err, tc.want)
			}
		})
	}
}

// TestSecretStrengthWarnsAboutWeakValues pins the NON-fatal half of the rule.
//
// A short or repetitive secret is a real weakness, but refusing to boot over
// one would take an existing instance down on the release that adds the check —
// and the operator could not fix it, because rotating ENCRYPTION_KEY makes
// everything already sealed under the old value unreadable. Bricking the system
// that notices outages, to strengthen a key that is already in use, is the wrong
// trade. So: it loads, and it complains every single startup.
func TestSecretStrengthWarnsAboutWeakValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"short encryption key", "ENCRYPTION_KEY", "hunter2"},
		{"short session secret", "SESSION_SECRET", "correct horse"},
		// 31 characters: one below the floor, otherwise perfectly random-looking.
		{"one character below the floor", "ENCRYPTION_KEY", strings.Repeat("aB3$", 7) + "xyz"},
		// Long enough, but length by repetition buys no entropy at all.
		{"one repeated character", "ENCRYPTION_KEY", strings.Repeat("a", 64)},
		{"two-character cycle", "SESSION_SECRET", strings.Repeat("ab", 32)},
		{"four-character cycle", "ENCRYPTION_KEY", strings.Repeat("aB3$", 16)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := required()
			env[tc.key] = tc.val
			cfg, err := Load(lookupMap(env))
			if err != nil {
				t.Fatalf("Load() refused a weak %s: %v — it must warn, not refuse", tc.key, err)
			}
			var named bool
			for _, warning := range cfg.Warnings {
				if strings.Contains(warning, tc.key) {
					named = true
				}
				// The complaint goes to stdout and the journal, so it must never
				// echo the value it is complaining about.
				if strings.Contains(warning, tc.val) {
					t.Fatalf("warning echoes the secret itself: %q", warning)
				}
			}
			if !named {
				t.Fatalf("a weak %s produced no warning naming it: %v", tc.key, cfg.Warnings)
			}
		})
	}
}

// A strong secret produces no warning at all, so the ones above mean something.
func TestStrongSecretsWarnAboutNothing(t *testing.T) {
	cfg, err := Load(lookupMap(required()))
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "SESSION_SECRET") || strings.Contains(warning, "ENCRYPTION_KEY") {
			t.Fatalf("a generated-looking secret was warned about: %q", warning)
		}
	}
}
