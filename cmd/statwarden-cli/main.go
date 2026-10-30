// Command statwarden-cli drives an Statwarden instance from a terminal.
//
// It authenticates with an API key against the REST API and never touches the
// database, so it works identically against a local dev server and a remote
// production instance.
//
// Configuration comes from the environment (or a permission-checked config
// file); the API key is deliberately not accepted as a flag, because argv is
// visible to every local user and is recorded in shell history.
//
//	export STATWARDEN_BASE_URL=https://statwarden.example
//	export STATWARDEN_API_KEY=...        # Settings > API keys, shown once
//	statwarden-cli monitor list
//
// Exit codes: 0 success, 1 request or transport failure, 2 usage error,
// 3 authentication or authorisation failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Ariazonaa/statwarden/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	// exitAuth separates "the credential is wrong" from "the server is
	// unreachable". Both used to be exit 1, which left a retry loop unable to
	// tell a transient outage (back off and try again) from a revoked key
	// (stop and page a human).
	exitAuth = 3
)

// usageError marks an error as the caller's mistake rather than a failed
// request, so main can map it to exit code 2 and a script can tell "I invoked
// this wrong" from "the server said no".
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// errHelpRequested reports that the user asked for help and got it. Asking for
// help is not a failure: main prints nothing further and exits 0. It is carried
// as an error only because that is how the parse path unwinds.
var errHelpRequested = errors.New("help requested")

func main() {
	err := run(os.Args[1:])
	code := exitCodeFor(err)
	if err != nil && !errors.Is(err, errHelpRequested) {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
	}
	os.Exit(code)
}

// exitCodeFor maps an error to the process exit code. It is a function so the
// mapping can be tested without spawning a process.
func exitCodeFor(err error) int {
	if err == nil || errors.Is(err, errHelpRequested) {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	switch statusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return exitAuth
	}
	return exitFailure
}

func run(args []string) error {
	if len(args) == 0 {
		// Nothing to run is a mistake, so the usage text is diagnostic output
		// and belongs on stderr alongside the error.
		printUsage(os.Stderr)
		return usageErrorf("no command given")
	}

	// Ctrl-C cancels the in-flight request rather than killing the process
	// mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "monitor", "monitors":
		return runMonitor(ctx, args[1:])
	case "version", "--version", "-version":
		fmt.Printf("statwarden-cli %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return nil
	case "help", "--help", "-h", "-help":
		// Requested help is the program's output, not a diagnostic: it goes to
		// stdout so `statwarden-cli --help | less` works.
		printUsage(os.Stdout)
		return nil
	default:
		printUsage(os.Stderr)
		return usageErrorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `statwarden-cli — manage an Statwarden instance from the terminal.

Usage:
  statwarden-cli monitor list    [flags]
  statwarden-cli monitor show    <id> [flags]
  statwarden-cli monitor create  --name NAME --type TYPE [flags]
  statwarden-cli monitor pause   <id>
  statwarden-cli monitor resume  <id>
  statwarden-cli monitor delete  <id> [--yes]
  statwarden-cli version

Configuration:
  STATWARDEN_BASE_URL   instance base URL, e.g. https://statwarden.example
  STATWARDEN_API_KEY    API key secret (Settings > API keys; shown once)
  STATWARDEN_CONFIG     optional config file path with {"base_url":…,"api_key":…}

Creating, pausing, resuming and deleting need a key with the `+"`write`"+` scope
owned by a user whose role is at least `+"`user`"+`.

Flags may be given before or after the positional id:
  statwarden-cli monitor delete 123 --yes
  statwarden-cli monitor delete --yes 123

Run any subcommand with --help for its flags.

Exit codes:
  0  success
  1  request or transport failure (server unreachable, 5xx, bad response)
  2  usage error (bad flags or arguments, missing or unreadable configuration)
  3  authentication or authorisation failure (HTTP 401 or 403): the key is
     unknown, revoked, expired, or lacks the scope or role required
`)
}

// commonFlags are the connection and output flags every subcommand accepts.
type commonFlags struct {
	baseURL  string
	insecure bool
	asJSON   bool
	timeout  time.Duration
}

func (cf *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&cf.baseURL, "base-url", "", "instance base URL (overrides STATWARDEN_BASE_URL)")
	fs.BoolVar(&cf.insecure, "insecure", false, "allow sending the API key over plaintext http to a non-loopback host")
	fs.BoolVar(&cf.asJSON, "json", false, "print the raw API response as JSON")
	fs.DurationVar(&cf.timeout, "timeout", defaultTimeout, "request timeout")
}

// connect resolves configuration and builds a client.
func (cf *commonFlags) connect() (*client, error) {
	s, err := loadSettings(cf.baseURL)
	if err != nil {
		return nil, err
	}
	if err := checkTransportSecurity(s.baseURL, cf.insecure); err != nil {
		return nil, err
	}
	return newClient(clientOptions{BaseURL: s.baseURL, APIKey: s.apiKey, Timeout: cf.timeout}), nil
}

// newFlagSet builds a subcommand flag set that reports usage errors instead of
// calling os.Exit, so every exit path runs through main's code mapping.
//
// Its own output is discarded: the flag package writes both help and parse
// diagnostics to the same writer, and those two need different destinations
// (stdout for help, stderr for errors). parseFlags routes them explicitly.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// reorderArgs moves flags ahead of positional arguments.
//
// The stdlib flag package stops parsing at the first non-flag argument, so
// `monitor delete 123 --yes` would leave --yes as a second positional and
// report "expected exactly one monitor id, got 2 arguments" — blaming the user
// for typing the exact form the help text advertises. Users put flags last, so
// both orders must work.
//
// An explicit `--` terminator still ends flag parsing: everything after it is
// positional, even if it looks like a flag.
func reorderArgs(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			flags = append(flags, "--")
			break
		}
		// A lone "-" is a conventional stdin placeholder, not a flag.
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)

		// -name=value carries its own value, and a boolean flag never
		// consumes the following argument. Anything else takes the next
		// argument as its value and must travel with it.
		if strings.Contains(arg, "=") {
			continue
		}
		def := fs.Lookup(strings.TrimLeft(arg, "-"))
		if def == nil {
			// Unknown flag, or -h/-help, which the flag package handles
			// itself. Guessing whether it takes a value would turn a clear
			// "flag provided but not defined" into a confusing one, so leave
			// it alone and let Parse produce the diagnostic.
			continue
		}
		if bf, ok := def.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(reorderArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printFlagHelp(os.Stdout, fs)
			return errHelpRequested
		}
		// The flag set's own writer is discarded, so echo the flag list here —
		// on stderr, where a diagnostic belongs.
		printFlagHelp(os.Stderr, fs)
		return usageErrorf("%s", err.Error())
	}
	return nil
}

// printFlagHelp writes a subcommand's flag list to w.
func printFlagHelp(w io.Writer, fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(w, "Usage: statwarden-cli %s [flags]\n\nFlags:\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

func runMonitor(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageErrorf("monitor: expected one of list, show, create, pause, resume, delete")
	}
	switch args[0] {
	case "list", "ls":
		return runMonitorList(ctx, args[1:])
	case "show", "get":
		return runMonitorShow(ctx, args[1:])
	case "create", "add":
		return runMonitorCreate(ctx, args[1:])
	case "pause":
		return runMonitorActive(ctx, "pause", args[1:], false)
	case "resume":
		return runMonitorActive(ctx, "resume", args[1:], true)
	case "delete", "rm":
		return runMonitorDelete(ctx, args[1:])
	case "help", "--help", "-h", "-help":
		// The top-level help promises "run any subcommand with --help"; that
		// promise has to hold for the `monitor` group itself, not just its
		// leaves.
		printMonitorUsage(os.Stdout)
		return nil
	default:
		return usageErrorf("monitor: unknown subcommand %q", args[0])
	}
}

func printMonitorUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `statwarden-cli monitor — inspect and manage monitors.

Usage:
  statwarden-cli monitor list    [flags]
  statwarden-cli monitor show    <id> [flags]
  statwarden-cli monitor create  --name NAME --type TYPE [flags]
  statwarden-cli monitor pause   <id> [flags]
  statwarden-cli monitor resume  <id> [flags]
  statwarden-cli monitor delete  <id> [--yes] [flags]

Aliases: list=ls, show=get, create=add, delete=rm.

Flags may be given before or after the positional id.
Run any of these with --help for its flags, e.g. statwarden-cli monitor list --help.
`)
}

func runMonitorList(ctx context.Context, args []string) error {
	fs := newFlagSet("monitor list")
	var cf commonFlags
	cf.register(fs)
	f := MonitorFilter{}
	fs.StringVar(&f.Status, "status", "", "filter by status (up, down, pending, paused)")
	fs.StringVar(&f.Type, "type", "", "filter by monitor type (http, tcp, ping, …)")
	fs.StringVar(&f.Search, "search", "", "filter by name substring")
	fs.IntVar(&f.Limit, "limit", 0, "page size (server default 50, max 200)")
	fs.IntVar(&f.Offset, "offset", 0, "page offset")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErrorf("monitor list: unexpected argument %q", fs.Arg(0))
	}

	c, err := cf.connect()
	if err != nil {
		return err
	}
	list, err := c.ListMonitors(ctx, f)
	if err != nil {
		return err
	}
	if cf.asJSON {
		return writeJSON(os.Stdout, list)
	}
	if err := writeMonitorTable(os.Stdout, list.Items); err != nil {
		return err
	}
	// Only worth saying when the page is not the whole set — otherwise it is
	// noise on every invocation.
	if list.Total > len(list.Items) {
		fmt.Printf("\nShowing %d of %d (use --limit/--offset to page).\n", len(list.Items), list.Total)
	}
	return nil
}

func runMonitorShow(ctx context.Context, args []string) error {
	fs := newFlagSet("monitor show")
	var cf commonFlags
	cf.register(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	id, err := requireID(fs, "monitor show")
	if err != nil {
		return err
	}

	c, err := cf.connect()
	if err != nil {
		return err
	}
	m, err := c.GetMonitor(ctx, id)
	if err != nil {
		return err
	}
	if cf.asJSON {
		return writeJSON(os.Stdout, m)
	}
	return writeMonitorDetail(os.Stdout, m)
}

func runMonitorCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("monitor create")
	var cf commonFlags
	cf.register(fs)

	var (
		name        string
		typ         string
		configRaw   string
		interval    int
		timeoutSecs int
		maxRetries  int
		description string
		linkURL     string
		paused      bool
	)
	fs.StringVar(&name, "name", "", "display name (required)")
	fs.StringVar(&typ, "type", "", "monitor type, e.g. http, tcp, ping (required)")
	fs.StringVar(&configRaw, "config", "", "type-specific config as JSON, or @path to read it from a file")
	fs.IntVar(&interval, "interval", 0, "check interval in seconds")
	fs.IntVar(&timeoutSecs, "timeout-seconds", 0, "per-check timeout in seconds")
	fs.IntVar(&maxRetries, "max-retries", 0, "retries before the monitor is declared down")
	fs.StringVar(&description, "description", "", "free-text description")
	fs.StringVar(&linkURL, "link-url", "", "link shown next to the monitor in the UI")
	fs.BoolVar(&paused, "paused", false, "create the monitor without starting it")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErrorf("monitor create: unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(name) == "" {
		return usageErrorf("monitor create: --name is required")
	}
	if strings.TrimSpace(typ) == "" {
		return usageErrorf("monitor create: --type is required")
	}

	cfg, err := readConfigArg(configRaw)
	if err != nil {
		return err
	}

	req := MonitorCreateRequest{
		Name:        name,
		Type:        typ,
		Config:      cfg,
		Interval:    interval,
		MaxRetries:  maxRetries,
		Description: description,
		LinkURL:     linkURL,
	}
	if timeoutSecs > 0 {
		req.Timeout = &timeoutSecs
	}
	// Only send `active` when the user asked for it; otherwise the server
	// applies its own default. Sending false unconditionally would silently
	// create every monitor paused.
	if paused {
		active := false
		req.Active = &active
	}

	c, err := cf.connect()
	if err != nil {
		return err
	}
	m, err := c.CreateMonitor(ctx, req)
	if err != nil {
		return annotateWriteError(err)
	}
	if cf.asJSON {
		return writeJSON(os.Stdout, m)
	}
	fmt.Printf("Created monitor %d (%s).\n\n", m.ID, m.Name)
	return writeMonitorDetail(os.Stdout, m)
}

func runMonitorActive(ctx context.Context, verb string, args []string, active bool) error {
	fs := newFlagSet("monitor " + verb)
	var cf commonFlags
	cf.register(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	id, err := requireID(fs, "monitor "+verb)
	if err != nil {
		return err
	}

	c, err := cf.connect()
	if err != nil {
		return err
	}
	if err := c.SetMonitorActive(ctx, id, active); err != nil {
		return annotateWriteError(err)
	}
	past := "Paused"
	if active {
		past = "Resumed"
	}
	fmt.Printf("%s monitor %d.\n", past, id)
	return nil
}

func runMonitorDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("monitor delete")
	var cf commonFlags
	cf.register(fs)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	id, err := requireID(fs, "monitor delete")
	if err != nil {
		return err
	}

	c, err := cf.connect()
	if err != nil {
		return err
	}

	if !yes {
		// Name the monitor in the prompt: confirming a bare id invites
		// deleting the wrong thing.
		m, err := c.GetMonitor(ctx, id)
		if err != nil {
			return err
		}
		// The wording tracks what the server actually does. Deleting is a SOFT
		// delete since migration 0018: the monitor and its history go to the
		// recycle bin and can be restored from the dashboard until the grace
		// period expires. Telling an operator "this cannot be undone" when it can
		// be is the kind of false alarm that teaches people to stop reading
		// prompts — including the ones that mean it.
		ok, err := confirm(fmt.Sprintf(
			"Move monitor %d (%s) to the recycle bin? Its history is kept and it can "+
				"be restored from the dashboard.", m.ID, m.Name))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted.")
			return nil
		}
	}

	if err := c.DeleteMonitor(ctx, id); err != nil {
		return annotateWriteError(err)
	}
	fmt.Printf("Moved monitor %d to the recycle bin.\n", id)
	return nil
}

// requireID reads the single positional id argument.
func requireID(fs *flag.FlagSet, cmd string) (int64, error) {
	switch fs.NArg() {
	case 0:
		return 0, usageErrorf("%s: expected a monitor id", cmd)
	case 1:
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id <= 0 {
			return 0, usageErrorf("%s: %q is not a valid monitor id", cmd, fs.Arg(0))
		}
		return id, nil
	default:
		// Flags are hoisted before positionals by reorderArgs, so anything
		// left here really is a surplus argument. Naming them saves the user
		// guessing which one was unexpected.
		return 0, usageErrorf("%s: expected exactly one monitor id, got %d: %s",
			cmd, fs.NArg(), strings.Join(fs.Args(), " "))
	}
}

// readConfigArg resolves --config, which is either inline JSON or @path.
func readConfigArg(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// Let the server apply its own default ({}) rather than duplicating
		// that decision here.
		return nil, nil
	}
	if strings.HasPrefix(raw, "@") {
		path := strings.TrimPrefix(raw, "@")
		b, err := os.ReadFile(path) //nolint:gosec // G304: --config @file names the file deliberately
		if err != nil {
			return nil, usageErrorf("--config: read %s: %v", path, err)
		}
		raw = string(b)
	}
	if !json.Valid([]byte(raw)) {
		return nil, usageErrorf("--config: not valid JSON (expected an object such as '{\"url\":\"https://example.com\"}')")
	}
	return json.RawMessage(raw), nil
}

// annotateWriteError adds the one piece of context the server cannot supply:
// which of the two independent permissions is likely missing.
func annotateWriteError(err error) error {
	if statusOf(err) == 403 {
		return fmt.Errorf("%w\nMint a key with the `write` scope, and check the owning user's role is at least `user`", err)
	}
	return err
}

// confirm asks a yes/no question on stdin. When stdin is not a terminal there
// is nobody to ask, so it refuses rather than silently proceeding — a piped
// delete must opt in with --yes.
func confirm(prompt string) (bool, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false, usageErrorf("stdin is not a terminal; pass --yes to confirm non-interactively")
	}
	fmt.Printf("%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		// A bare newline is a valid "no", and Scanln reports it as an error.
		return false, nil
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
