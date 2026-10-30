package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckTransportSecurity(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		insecure bool
		wantErr  bool
	}{
		{"https is always fine", "https://statwarden.example", false, false},
		{"plaintext to a public host is refused", "http://statwarden.example", false, true},
		{"plaintext to localhost is fine", "http://localhost:8080", false, false},
		{"plaintext to 127.0.0.1 is fine", "http://127.0.0.1:8080", false, false},
		{"plaintext to ::1 is fine", "http://[::1]:8080", false, false},
		{"--insecure overrides the refusal", "http://statwarden.example", true, false},
		{"a non-http scheme is rejected", "ftp://statwarden.example", false, true},
		{"a missing host is rejected", "https://", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTransportSecurity(tc.baseURL, tc.insecure)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkTransportSecurity(%q, %v) = %v, wantErr %v", tc.baseURL, tc.insecure, err, tc.wantErr)
			}
			// A misconfiguration is the caller's mistake, so it must exit 2
			// rather than 1 — a script retrying on "request failed" would
			// otherwise loop forever on a bad base URL.
			if err != nil {
				var ue *usageError
				if !asUsageError(err, &ue) {
					t.Errorf("checkTransportSecurity(%q) returned %T, want *usageError so main exits 2", tc.baseURL, err)
				}
			}
		})
	}
}

// useEmptyConfigFile points STATWARDEN_CONFIG at an existing but empty config file.
//
// Tests cannot simply leave STATWARDEN_CONFIG unset: loadSettings would then fall
// back to the real <user config dir>/statwarden/cli.json and pick up whatever the
// developer running the suite happens to have. Nor can they point it at an
// absent path, which is now (deliberately) an error.
func useEmptyConfigFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATWARDEN_CONFIG", path)
}

func TestLoadSettingsFromEnvironment(t *testing.T) {
	useEmptyConfigFile(t)
	t.Setenv("STATWARDEN_BASE_URL", "https://statwarden.example/")
	t.Setenv("STATWARDEN_API_KEY", "secret")

	s, err := loadSettings("")
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.baseURL != "https://statwarden.example" {
		t.Errorf("baseURL = %q, want the trailing slash stripped", s.baseURL)
	}
	if s.apiKey != "secret" {
		t.Errorf("apiKey = %q, want secret", s.apiKey)
	}
}

// TestLoadSettingsFallsBackToPreRenameVariables: scripts written while the
// project was called Uptura export UPTURA_*; they keep working, and the new
// STATWARDEN_* name wins wherever both are set.
func TestLoadSettingsFallsBackToPreRenameVariables(t *testing.T) {
	useEmptyConfigFile(t)
	t.Setenv("STATWARDEN_BASE_URL", "")
	t.Setenv("STATWARDEN_API_KEY", "")
	t.Setenv("UPTURA_BASE_URL", "https://legacy.example")
	t.Setenv("UPTURA_API_KEY", "legacy-secret")

	s, err := loadSettings("")
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.baseURL != "https://legacy.example" || s.apiKey != "legacy-secret" {
		t.Fatalf("got %q / %q, want the UPTURA_* values", s.baseURL, s.apiKey)
	}

	t.Setenv("STATWARDEN_API_KEY", "new-secret")
	if s, err = loadSettings(""); err != nil || s.apiKey != "new-secret" {
		t.Fatalf("got %q (err %v), want STATWARDEN_API_KEY to win over UPTURA_API_KEY", s.apiKey, err)
	}
}

func TestLoadSettingsFlagOverridesEnvironment(t *testing.T) {
	useEmptyConfigFile(t)
	t.Setenv("STATWARDEN_BASE_URL", "https://env.example")
	t.Setenv("STATWARDEN_API_KEY", "secret")

	s, err := loadSettings("https://flag.example")
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.baseURL != "https://flag.example" {
		t.Errorf("baseURL = %q, want the --base-url flag to win", s.baseURL)
	}
}

func TestLoadSettingsMissingKeyIsAUsageError(t *testing.T) {
	useEmptyConfigFile(t)
	t.Setenv("STATWARDEN_BASE_URL", "https://statwarden.example")
	t.Setenv("STATWARDEN_API_KEY", "")

	_, err := loadSettings("")
	if err == nil {
		t.Fatal("loadSettings: expected an error when no API key is configured")
	}
	if !strings.Contains(err.Error(), "STATWARDEN_API_KEY") {
		t.Errorf("error = %q, want it to name the environment variable", err)
	}
}

func TestLoadSettingsReadsConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(path, []byte(`{"base_url":"https://file.example","api_key":"filekey"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATWARDEN_CONFIG", path)
	t.Setenv("STATWARDEN_BASE_URL", "")
	t.Setenv("STATWARDEN_API_KEY", "")

	s, err := loadSettings("")
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.baseURL != "https://file.example" || s.apiKey != "filekey" {
		t.Errorf("settings = %+v, want the file values", s)
	}
}

func TestLoadSettingsRejectsWorldReadableConfigFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(path, []byte(`{"api_key":"filekey"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATWARDEN_CONFIG", path)

	_, err := loadSettings("")
	if err == nil {
		t.Fatal("loadSettings: expected an error for a group/world-readable config file holding a bearer token")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error = %q, want it to suggest the fix", err)
	}
	var ue *usageError
	if !asUsageError(err, &ue) {
		t.Errorf("error is %T, want *usageError so main exits 2", err)
	}
}

// TestLoadSettingsReportsUnusableConfigPaths pins defect 5: every way a config
// file can be unusable must be a usage error (exit 2), and a path the user
// explicitly set must never be skipped in silence.
func TestLoadSettingsReportsUnusableConfigPaths(t *testing.T) {
	dir := t.TempDir()

	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{nope`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory stands in for "path exists but cannot be read as a file",
	// which is reproducible on every OS.
	unreadable := filepath.Join(dir, "unreadable.json")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "typo.json")

	tests := []struct {
		name   string
		path   string
		wantIn string
	}{
		{"malformed json", malformed, "cannot parse config file"},
		{"unreadable path", unreadable, "config file"},
		{
			// Previously ignored, surfacing much later as "no base URL",
			// which never hinted that the config file had been skipped.
			name:   "a path set in STATWARDEN_CONFIG that does not exist",
			path:   absent,
			wantIn: "does not exist",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STATWARDEN_CONFIG", tc.path)
			t.Setenv("STATWARDEN_BASE_URL", "https://statwarden.example")
			t.Setenv("STATWARDEN_API_KEY", "secret")

			_, err := loadSettings("")
			if err == nil {
				t.Fatalf("loadSettings with STATWARDEN_CONFIG=%s: expected an error", tc.path)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantIn)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error = %q, want it to name the offending path %q", err, tc.path)
			}
			var ue *usageError
			if !asUsageError(err, &ue) {
				t.Errorf("error is %T, want *usageError so main exits 2 consistently for every config problem", err)
			}
			if got := exitCodeFor(err); got != exitUsage {
				t.Errorf("exitCodeFor = %d, want %d", got, exitUsage)
			}
		})
	}
}

// A missing file at the DEFAULT location stays non-fatal: the environment on
// its own is a complete configuration.
func TestLoadSettingsToleratesAbsentDefaultConfigFile(t *testing.T) {
	t.Setenv("STATWARDEN_CONFIG", "")
	t.Setenv("STATWARDEN_BASE_URL", "https://statwarden.example")
	t.Setenv("STATWARDEN_API_KEY", "secret")

	path, explicit, err := configPath()
	if err != nil {
		t.Skipf("no user config dir on this machine: %v", err)
	}
	if explicit {
		t.Fatal("configPath reported an explicit path although STATWARDEN_CONFIG is empty")
	}
	if _, err := os.Stat(path); err == nil {
		t.Skipf("the machine running this test has a real config file at %s", path)
	}

	if _, err := loadSettings(""); err != nil {
		t.Fatalf("loadSettings: %v, want an absent default config file to be tolerated", err)
	}
}

func TestReadConfigArg(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(good, []byte(`{"url":"https://example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty is omitted so the server defaults it", "", "", false},
		{"inline json passes through", `{"url":"https://x"}`, `{"url":"https://x"}`, false},
		{"@file is read", "@" + good, `{"url":"https://example.com"}`, false},
		{"invalid json is rejected", `{nope`, "", true},
		{"a missing file is rejected", "@" + filepath.Join(dir, "nope.json"), "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readConfigArg(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("readConfigArg(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && string(got) != tc.want {
				t.Errorf("readConfigArg(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunReportsUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"frobnicate"}},
		{"monitor with no subcommand", []string{"monitor"}},
		{"unknown monitor subcommand", []string{"monitor", "frobnicate"}},
		{"show without an id", []string{"monitor", "show"}},
		{"show with a non-numeric id", []string{"monitor", "show", "abc"}},
		{"create without --name", []string{"monitor", "create", "--type", "http"}},
		{"create without --type", []string{"monitor", "create", "--name", "n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil {
				t.Fatalf("run(%v) = nil, want a usage error", tc.args)
			}
			var ue *usageError
			if !asUsageError(err, &ue) {
				t.Fatalf("run(%v) returned %T (%v), want *usageError so main exits 2", tc.args, err, err)
			}
		})
	}
}

// asUsageError is errors.As specialised to *usageError, kept local so the test
// reads as a single assertion.
func asUsageError(err error, target **usageError) bool {
	for err != nil {
		if ue, ok := err.(*usageError); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
