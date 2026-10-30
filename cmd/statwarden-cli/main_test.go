package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureOutput swaps os.Stdout and os.Stderr for pipes while fn runs.
//
// The commands print through the package-level os.Stdout/os.Stderr rather than
// an injected writer, and the whole point of several of these tests is WHICH
// stream something lands on, so the streams themselves have to be observed.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	var wg sync.WaitGroup
	var outBuf, errBuf bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	func() {
		// Restore on panic too, or a failing test takes the whole suite's
		// output down with it.
		defer func() {
			os.Stdout, os.Stderr = origOut, origErr
			_ = outW.Close()
			_ = errW.Close()
		}()
		fn()
	}()

	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()
	return outBuf.String(), errBuf.String()
}

// testEnv points the CLI at srv with a known API key and an empty config file,
// so no test reads the developer's real ~/.config/statwarden/cli.json.
func testEnv(t *testing.T, baseURL, apiKey string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATWARDEN_CONFIG", path)
	t.Setenv("STATWARDEN_BASE_URL", baseURL)
	t.Setenv("STATWARDEN_API_KEY", apiKey)
}

// -----------------------------------------------------------------------------
// Defect 1: flags after a positional argument were silently dropped.
// -----------------------------------------------------------------------------

func TestReorderArgsHoistsFlagsAheadOfPositionals(t *testing.T) {
	// A flag set shaped like `monitor delete`: one bool, one string, one
	// duration, so the value-consuming cases are all covered.
	newFS := func() *flag.FlagSet {
		fs := newFlagSet("test")
		fs.Bool("yes", false, "")
		fs.Bool("json", false, "")
		fs.String("base-url", "", "")
		fs.Duration("timeout", 0, "")
		return fs
	}

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"already in flag-first order", []string{"--yes", "123"}, []string{"--yes", "123"}},
		{"bool flag after the id", []string{"123", "--yes"}, []string{"--yes", "123"}},
		{"single-dash bool flag after the id", []string{"123", "-json"}, []string{"-json", "123"}},
		{
			// The value must travel with its flag, or "5s" becomes a second
			// positional and --timeout swallows the id.
			name: "value flag after the id keeps its value",
			in:   []string{"123", "-timeout", "5s"},
			want: []string{"-timeout", "5s", "123"},
		},
		{"-flag=value form needs no lookahead", []string{"123", "--timeout=5s"}, []string{"--timeout=5s", "123"}},
		{"flags on both sides of the id", []string{"-json", "123", "--yes"}, []string{"-json", "--yes", "123"}},
		{"two positionals are both preserved", []string{"12", "34"}, []string{"12", "34"}},
		{"nothing to do", nil, nil},
		{
			// An explicit terminator must keep its meaning: everything after
			// it is positional even if it looks like a flag.
			name: "-- ends flag parsing",
			in:   []string{"--yes", "--", "-json"},
			want: []string{"--yes", "--", "-json"},
		},
		{"a lone dash is positional", []string{"-", "--yes"}, []string{"--yes", "-"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reorderArgs(newFS(), tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("reorderArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("reorderArgs(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

// TestFlagsWorkOnBothSidesOfThePositionalID drives the real commands end to
// end. `statwarden-cli monitor delete <id> --yes` is the exact form the built-in
// help advertises, and it used to fail with "expected exactly one monitor id,
// got 2 arguments".
func TestFlagsWorkOnBothSidesOfThePositionalID(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
	}{
		{"delete: flag after the id", []string{"monitor", "delete", "123", "--yes"}, "DELETE", "/api/v1/monitors/123"},
		{"delete: flag before the id", []string{"monitor", "delete", "--yes", "123"}, "DELETE", "/api/v1/monitors/123"},
		{"show: single-dash flag after the id", []string{"monitor", "show", "123", "-json"}, "GET", "/api/v1/monitors/123"},
		{"show: single-dash flag before the id", []string{"monitor", "show", "-json", "123"}, "GET", "/api/v1/monitors/123"},
		{"pause: value flag after the id", []string{"monitor", "pause", "123", "-timeout", "5s"}, "POST", "/api/v1/monitors/123/pause"},
		{"pause: value flag before the id", []string{"monitor", "pause", "-timeout", "5s", "123"}, "POST", "/api/v1/monitors/123/pause"},
		{"resume: flag after the id", []string{"monitor", "resume", "123", "--json"}, "POST", "/api/v1/monitors/123/resume"},
		{"delete: -- terminator before the id", []string{"monitor", "delete", "--yes", "--", "123"}, "DELETE", "/api/v1/monitors/123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				_, _ = io.WriteString(w, `{"id":123,"name":"api","type":"http","active":true}`)
			}))
			defer srv.Close()
			testEnv(t, srv.URL, "test-secret")

			var err error
			captureOutput(t, func() { err = run(tc.args) })
			if err != nil {
				t.Fatalf("run(%q) = %v, want success", tc.args, err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", gotMethod, gotPath, tc.wantMethod, tc.wantPath)
			}
		})
	}
}

// A trailing flag no longer being a positional must not weaken the check that
// catches a genuinely surplus argument.
func TestTwoIDsIsStillReportedAsTwoIDs(t *testing.T) {
	tests := [][]string{
		{"monitor", "delete", "12", "34"},
		{"monitor", "show", "12", "34"},
		{"monitor", "pause", "12", "34", "--json"},
		{"monitor", "delete", "--yes", "12", "34"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// No server: reaching the network would itself be the bug.
			testEnv(t, "http://127.0.0.1:1", "test-secret")

			var err error
			captureOutput(t, func() { err = run(args) })
			if err == nil {
				t.Fatalf("run(%q) = nil, want a usage error", args)
			}
			if !strings.Contains(err.Error(), "expected exactly one monitor id") {
				t.Errorf("error = %q, want it to report two ids", err)
			}
			if got := exitCodeFor(err); got != exitUsage {
				t.Errorf("exitCodeFor = %d, want %d", got, exitUsage)
			}
		})
	}
}

func TestUnknownFlagIsStillAUsageError(t *testing.T) {
	testEnv(t, "http://127.0.0.1:1", "test-secret")

	var err error
	_, stderr := captureOutput(t, func() { err = run([]string{"monitor", "show", "123", "--frobnicate"}) })
	if err == nil {
		t.Fatal("run: expected an error for an undefined flag")
	}
	if got := exitCodeFor(err); got != exitUsage {
		t.Errorf("exitCodeFor = %d, want %d", got, exitUsage)
	}
	// The flag list is a diagnostic here, so it belongs on stderr.
	if !strings.Contains(stderr, "-json") {
		t.Errorf("stderr = %q, want the flag list on stderr for a bad flag", stderr)
	}
}

// -----------------------------------------------------------------------------
// Defect 2: asking for help was treated as an error.
// -----------------------------------------------------------------------------

func TestHelpGoesToStdoutAndExitsZero(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"top level --help", []string{"--help"}, "statwarden-cli monitor list"},
		{"top level -h", []string{"-h"}, "statwarden-cli monitor list"},
		{"top level help verb", []string{"help"}, "statwarden-cli monitor list"},
		{"subcommand group", []string{"monitor", "--help"}, "statwarden-cli monitor show"},
		{"subcommand group help verb", []string{"monitor", "help"}, "statwarden-cli monitor show"},
		{"leaf subcommand", []string{"monitor", "list", "--help"}, "-status"},
		{"leaf subcommand -h", []string{"monitor", "show", "-h"}, "-json"},
		{"leaf subcommand with flags", []string{"monitor", "delete", "--help"}, "-yes"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately hostile configuration: asking for help must never
			// need a reachable server or a valid config.
			t.Setenv("STATWARDEN_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
			t.Setenv("STATWARDEN_BASE_URL", "")
			t.Setenv("STATWARDEN_API_KEY", "")

			var err error
			stdout, stderr := captureOutput(t, func() { err = run(tc.args) })

			if got := exitCodeFor(err); got != exitOK {
				t.Errorf("exitCodeFor(run(%q)) = %d (%v), want 0 — asking for help is not an error", tc.args, got, err)
			}
			if !strings.Contains(stdout, tc.wantIn) {
				t.Errorf("stdout = %q, want it to contain %q — help must be pipeable, e.g. `statwarden-cli --help | less`", stdout, tc.wantIn)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want help to write nothing to stderr", stderr)
			}
		})
	}
}

// The top-level help promises this, so it has to be true.
func TestTopLevelHelpAdvertisesWhatWorks(t *testing.T) {
	var buf bytes.Buffer
	printUsage(&buf)
	help := buf.String()

	for _, want := range []string{
		"Run any subcommand with --help",
		"monitor delete  <id> [--yes]",
		// Defect 3: every exit code must be documented.
		"0  success",
		"1  request or transport failure",
		"2  usage error",
		"3  authentication or authorisation failure",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("top-level help does not mention %q", want)
		}
	}
}

// -----------------------------------------------------------------------------
// Defect 3: auth failure was indistinguishable from network failure.
// -----------------------------------------------------------------------------

func TestExitCodeForClassifiesErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, exitOK},
		{"help is not a failure", errHelpRequested, exitOK},
		{"usage error", usageErrorf("bad flag"), exitUsage},
		{"wrapped usage error", fmt.Errorf("context: %w", usageErrorf("bad flag")), exitUsage},

		{"401 is an auth failure", &APIError{Status: 401}, exitAuth},
		{"403 is an auth failure", &APIError{Status: 403}, exitAuth},
		{"403 stays an auth failure once annotated", annotateWriteError(&APIError{Status: 403}), exitAuth},

		{"400 is a request failure", &APIError{Status: 400}, exitFailure},
		{"404 is a request failure", &APIError{Status: 404}, exitFailure},
		{"500 is a request failure", &APIError{Status: 500}, exitFailure},
		{"502 is a request failure", &APIError{Status: 502}, exitFailure},
		{
			// The whole point of exitAuth: a retry loop must be able to tell
			// "back off, the server is down" from "stop, the key is dead".
			name: "an unreachable server is a request failure, not an auth failure",
			err:  &transportError{Method: "GET", URL: "http://x/api/v1/monitors", Err: errors.New("dial tcp: connection refused")},
			want: exitFailure,
		},
		{"an unclassified error is a request failure", errors.New("boom"), exitFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestAuthFailureExitCodeEndToEnd drives the mapping through a real request, so
// a future change to error wrapping cannot quietly collapse 401 back into 1.
func TestAuthFailureExitCodeEndToEnd(t *testing.T) {
	tests := []struct {
		name   string
		status int
		args   []string
		want   int
	}{
		{"401 on a read", 401, []string{"monitor", "list"}, exitAuth},
		{"403 on a write", 403, []string{"monitor", "delete", "1", "--yes"}, exitAuth},
		{"500 stays a request failure", 500, []string{"monitor", "list"}, exitFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"title":"nope","detail":"nope"}`)
			}))
			defer srv.Close()
			testEnv(t, srv.URL, "test-secret")

			var err error
			captureOutput(t, func() { err = run(tc.args) })
			if err == nil {
				t.Fatalf("run(%q) = nil, want an error", tc.args)
			}
			if got := exitCodeFor(err); got != tc.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", err, got, tc.want)
			}
		})
	}
}

func TestUnreachableServerExitsOneNotThree(t *testing.T) {
	// A port nothing is listening on: the closed httptest server frees it.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()
	testEnv(t, dead, "test-secret")

	var err error
	captureOutput(t, func() { err = run([]string{"monitor", "list"}) })
	if err == nil {
		t.Fatal("run: expected an error against a closed port")
	}
	if got := exitCodeFor(err); got != exitFailure {
		t.Errorf("exitCodeFor(%v) = %d, want %d — a down server must not look like a revoked key", err, got, exitFailure)
	}
}

// -----------------------------------------------------------------------------
// The API key must never reach an output stream.
// -----------------------------------------------------------------------------

// TestAPIKeyNeverReachesOutput is a regression guard for the design decision
// that there is no --api-key flag: the secret comes from the environment or a
// permission-checked file and must stay there. It has to hold on the success
// path, on the JSON path (which re-encodes whatever the server sent) and on
// every error path, including one where the server echoes the key back.
func TestAPIKeyNeverReachesOutput(t *testing.T) {
	const secret = "statwarden_sk_do_not_print_me_9f3a1c"

	tests := []struct {
		name    string
		handler http.HandlerFunc
		args    []string
	}{
		{
			name: "list table output",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"items":[{"id":1,"name":"api","type":"http","active":true}],"total":1}`)
			},
			args: []string{"monitor", "list"},
		},
		{
			name: "list json output",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"items":[{"id":1,"name":"api","type":"http","active":true}],"total":1}`)
			},
			args: []string{"monitor", "list", "--json"},
		},
		{
			name: "show json output",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"id":1,"name":"api","type":"http","active":true}`)
			},
			args: []string{"monitor", "show", "1", "--json"},
		},
		{
			// A hostile or careless server reflecting the Authorization header
			// into its error body must not get it printed either.
			name: "the server echoes the key back in an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"title":"Unauthorized","detail":"bad token `+secret+`"}`)
			},
			args: []string{"monitor", "list"},
		},
		{
			name: "create write error path",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"title":"Forbidden","detail":"missing scope"}`)
			},
			args: []string{"monitor", "create", "--name", "n", "--type", "http"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAuth = r.Header.Get("Authorization")
				tc.handler(w, r)
			}))
			defer srv.Close()
			testEnv(t, srv.URL, secret)

			var err error
			stdout, stderr := captureOutput(t, func() {
				err = run(tc.args)
				if err != nil {
					// Mirror what main prints, so the assertion covers the
					// rendered error and not just the streams.
					fmt.Fprintln(os.Stderr, "error: "+err.Error())
				}
			})

			// Guard the guard: if the key never went out, a passing assertion
			// below would prove nothing.
			if sawAuth != "Bearer "+secret {
				t.Fatalf("server saw Authorization %q, want the key to have been sent", sawAuth)
			}
			if strings.Contains(stdout, secret) {
				t.Errorf("the API key leaked to stdout: %q", stdout)
			}
			if strings.Contains(stderr, secret) {
				t.Errorf("the API key leaked to stderr: %q", stderr)
			}
		})
	}
}

// The absence of an --api-key flag is deliberate: argv is world-readable
// through ps/procfs and is recorded in shell history.
func TestNoAPIKeyFlagExists(t *testing.T) {
	fs := newFlagSet("monitor list")
	var cf commonFlags
	cf.register(fs)

	for _, name := range []string{"api-key", "apikey", "key", "token", "api_key"} {
		if fs.Lookup(name) != nil {
			t.Errorf("flag -%s exists; the API key must never be accepted on the command line "+
				"(argv is visible via ps and recorded in shell history)", name)
		}
	}
}

func TestMissingAPIKeyExplainsWhyThereIsNoFlag(t *testing.T) {
	useEmptyConfigFile(t)
	t.Setenv("STATWARDEN_BASE_URL", "https://statwarden.example")
	t.Setenv("STATWARDEN_API_KEY", "")

	_, err := loadSettings("")
	if err == nil {
		t.Fatal("loadSettings: expected an error with no API key")
	}
	if !strings.Contains(err.Error(), "ps and shell history") {
		t.Errorf("error = %q, want it to explain why there is no --api-key flag", err)
	}
}
