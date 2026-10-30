package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// settings is the resolved connection configuration: where to talk to and with
// which credential.
type settings struct {
	baseURL string
	apiKey  string
}

// fileSettings is the on-disk config file shape. It deliberately mirrors the
// two environment variables and nothing more — a CLI config file that grows
// options is a second source of truth nobody remembers to check.
type fileSettings struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// configPath returns the config file location: $STATWARDEN_CONFIG if set, else
// <user config dir>/statwarden/cli.json.
//
// explicit reports whether the path came from STATWARDEN_CONFIG. That distinction
// decides what a missing file means: the default location is merely a place a
// config file may live, but a path the user typed and that does not exist is a
// typo worth reporting rather than skipping.
func configPath() (path string, explicit bool, err error) {
	if p := envSetting("CONFIG"); p != "" {
		return p, true, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(dir, "statwarden", "cli.json")
	// The project was called Uptura before; a config written then still lives
	// under the old directory. Read it until the user moves it, rather than
	// silently starting without the stored base URL and key.
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		legacy := filepath.Join(dir, "uptura", "cli.json")
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			return legacy, false, nil
		}
	}
	return path, false, nil
}

// envSetting reads STATWARDEN_<name>, falling back to the pre-rename
// UPTURA_<name> so scripts written before the rename keep working.
func envSetting(name string) string {
	if v := strings.TrimSpace(os.Getenv("STATWARDEN_" + name)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("UPTURA_" + name))
}

// loadSettings resolves the base URL and API key. Environment variables win
// over the config file so a shell can override a stored default per command.
//
// The API key is NEVER accepted as a command-line flag: argv is readable by any
// local user through ps/procfs and is recorded in shell history. Env var or
// permission-checked file only.
func loadSettings(baseURLFlag string) (settings, error) {
	var s settings

	// A path that cannot even be determined is only fatal when the user asked
	// for one; otherwise the environment alone is a complete configuration.
	path, explicit, perr := configPath()
	if perr != nil && explicit {
		return s, usageErrorf("STATWARDEN_CONFIG: %v", perr)
	}
	if perr == nil {
		fs, err := readConfigFile(path, explicit)
		if err != nil {
			return s, err
		}
		s.baseURL = fs.BaseURL
		s.apiKey = fs.APIKey
	}

	if v := envSetting("BASE_URL"); v != "" {
		s.baseURL = v
	}
	if v := envSetting("API_KEY"); v != "" {
		s.apiKey = v
	}
	if v := strings.TrimSpace(baseURLFlag); v != "" {
		s.baseURL = v
	}

	if s.baseURL == "" {
		return s, usageErrorf("no base URL: set STATWARDEN_BASE_URL (e.g. https://statwarden.example) or pass --base-url")
	}
	if s.apiKey == "" {
		return s, usageErrorf("no API key: set STATWARDEN_API_KEY.\n" +
			"Mint one in the web UI under Settings > API keys; the secret is shown exactly once.\n" +
			"The key is read from the environment on purpose — a --api-key flag would leak it to ps and shell history.")
	}

	s.baseURL = strings.TrimRight(s.baseURL, "/")
	return s, nil
}

// readConfigFile loads the config file. A missing file at the DEFAULT location
// is not an error — the environment alone is a complete configuration — but a
// missing file at a path the user set in STATWARDEN_CONFIG is, because silently
// skipping it turns a typo into the baffling "no base URL" further down.
//
// Every problem here is a usage error (exit 2), not a request failure (exit 1):
// nothing has been sent yet, and the fix is always to the caller's environment.
// Malformed JSON and an unreadable path used to disagree about this.
//
// On non-Windows the file must not be group- or world-readable: it holds a
// bearer token that is equivalent to the user's API access. Windows has no
// meaningful Unix mode bits, so the check is skipped there rather than passing
// silently or failing spuriously.
func readConfigFile(path string, explicit bool) (fileSettings, error) {
	var fs fileSettings
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return fs, usageErrorf("STATWARDEN_CONFIG points at %s, which does not exist; "+
					"create it, correct the path, or unset STATWARDEN_CONFIG to use the environment alone", path)
			}
			return fs, nil
		}
		return fs, usageErrorf("cannot read config file %s: %v", path, err)
	}
	if info.IsDir() {
		return fs, usageErrorf("config file %s is a directory, not a file", path)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return fs, usageErrorf("config file %s is readable by other users (mode %04o); run: chmod 600 %s", path, perm, path)
		}
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a CLI reading the config file the user named is the point; the mode check is above
	if err != nil {
		return fs, usageErrorf("cannot read config file %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, &fs); err != nil {
		return fs, usageErrorf("cannot parse config file %s: %v", path, err)
	}
	return fs, nil
}

// checkTransportSecurity refuses to put a bearer token on the wire in the clear.
// Plain http is allowed only to loopback, where there is no network to sniff,
// or when the user has explicitly opted out. This mirrors the server's own
// posture: it enables Secure cookies exactly when PUBLIC_BASE_URL is https.
func checkTransportSecurity(baseURL string, insecure bool) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return usageErrorf("invalid base URL %q: %v", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return usageErrorf("invalid base URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return usageErrorf("invalid base URL %q: missing host", baseURL)
	}
	if u.Scheme == "https" || insecure || isLoopbackHost(u.Hostname()) {
		return nil
	}
	return usageErrorf("refusing to send the API key over plaintext http to %q; use https, or pass --insecure if you accept the risk", u.Host)
}

// isLoopbackHost reports whether host is a loopback name or literal.
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
