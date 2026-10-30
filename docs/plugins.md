# Writing a plugin

Statwarden's monitor types and notification providers are plugins behind two small
interfaces. Adding one never touches core code: you write a package, register it
from `init()`, and add a blank import. The scheduler and the API only ever see
the type-name string stored on the row.

There is one non-obvious step. The backend contract is not the whole contract —
the frontend needs a field spec too, and a Go test enforces that the two agree.
[Step 4](#4-declare-the-form-fields) covers it. Skip it and your type will load,
validate and probe correctly while being impossible to configure in the UI.

---

## Contributing a monitor type

### 1. The interface

```go
type Checker interface {
	// Type returns the unique registry name stored in monitors.type.
	Type() string
	// Validate checks a monitor's jsonb config and returns a helpful error
	// when it is unusable. It must not perform I/O.
	Validate(cfg json.RawMessage) error
	// Check executes one probe. It must honour ctx (the engine applies the
	// per-check timeout there) and report reachability via
	// CheckResult.Status; an error return means the check itself could not
	// run and is also treated as DOWN.
	Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error)
}
```

Two optional interfaces sit alongside it:

| Interface | Method | Meaning |
| --- | --- | --- |
| `registry.TimeoutDefaulter` | `DefaultTimeout() time.Duration` | per-type default when the monitor sets none |
| `registry.PassiveChecker` | `Passive()` | marker: the type is not probed, it is fed (see `pushcheck`) |

Three rules the engine relies on:

- **`Validate` must not perform I/O.** It runs on every save, including from the
  import path. A DNS lookup or a dial there turns a config check into a network
  round-trip and can hang a request.
- **`Check` must honour `ctx`.** The engine applies the per-check timeout by
  cancelling it. A check that ignores `ctx` holds a worker past its deadline and
  starves the pool.
- **A returned error means "the check could not run"** and is recorded as DOWN,
  same as a negative result. Use `CheckResult.Status` for "ran fine, target is
  down" and an error only for "I could not tell".

### 2. The package

Create `internal/monitor/types/<name>check/`. Keep the config in its own file
with its own tests — every existing type does, and it is what makes `Validate`
cheap to test without a network.

```go
// Package echocheck implements a demonstration monitor type: it dials a TCP
// address, writes a probe string and reports UP when the peer echoes it back.
package echocheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "echo"

const defaultTimeout = 10 * time.Second

// Config is the jsonb payload for an echo monitor. The json tags ARE the
// contract: the frontend sends exactly these names and the server rejects
// anything else, so renaming one is a breaking change.
type Config struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Payload string `json:"payload,omitempty"`
}

func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("echo: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields() // a typo'd field is a config error, not a silent default
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("echo: invalid config: %w", err)
	}
	return c, nil
}

func (c Config) validate() error {
	if c.Host == "" {
		return errors.New("host is required")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

// Checker implements registry.Checker.
type Checker struct{}

func init() { registry.Register(&Checker{}) }

func (*Checker) Type() string                   { return TypeName }
func (*Checker) DefaultTimeout() time.Duration  { return defaultTimeout }

func (*Checker) Validate(cfg json.RawMessage) error {
	c, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	return c.validate()
}

func (*Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	payload := c.Payload
	if payload == "" {
		payload = "ping"
	}

	start := time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if err != nil {
		// Could not reach it at all: report DOWN with the reason rather than
		// returning an error, because we DID manage to run the check.
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: time.Since(start),
			Message: err.Error(),
		}, nil
	}
	defer conn.Close() //nolint:errcheck // best-effort close on a probe socket

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Latency: time.Since(start), Message: err.Error()}, nil
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Latency: time.Since(start), Message: err.Error()}, nil
	}
	if string(buf) != payload {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: time.Since(start),
			Message: "peer did not echo the payload",
		}, nil
	}
	return domain.CheckResult{Status: domain.StatusUp, Latency: time.Since(start)}, nil
}
```

### 3. Register the package

`init()` self-registration only runs if the package is linked in. Add a blank
import to `cmd/statwarden/main.go`, in the existing alphabetical block:

```go
_ "github.com/Ariazonaa/statwarden/internal/monitor/types/echocheck"
```

### 4. Declare the form fields

This is the step that is easy to miss. The registry endpoint returns only the
type name — it carries no field metadata — so the dashboard renders its form
from `web/src/lib/api/registry.ts`. Add an entry whose field `key` values match
your `json` tags **exactly**:

```ts
{
	type: 'echo',
	labelKey: 'monitorType.echo',
	family: 'network',
	descriptionKey: 'monitorType.echoDesc',
	fields: [
		{ key: 'host', labelKey: 'field.host', type: 'text', required: true },
		{ key: 'port', labelKey: 'field.port', type: 'number', default: 7 },
		{ key: 'payload', labelKey: 'field.payload', type: 'text' }
	]
}
```

`internal/api/registry_contract_test.go` reflects over your `Config` struct and
fails if `registry.ts` declares a key your type does not accept:

```
registry_contract_test.go:168: monitor type "echo": registry.ts declares field key
    "hostname", but echocheck.Config has no such json tag. Accepted tags: host, payload, port
```

The check is one-directional on purpose: a Go field the UI chooses not to expose
is fine, a UI field the server rejects is not. Add the type to that test's table
as well, or it will tell you the type is unchecked.

Non-scalar fields need a `shape` so the form converts on submit — a text input
that produces `"200-299"` where Go wants an array of numbers will be rejected.
See `accepted_status_codes` in `httpcheck` for the worked case.

Add the label keys to **both** `web/src/lib/i18n/locales/en.ts` and `de.ts`;
`de.ts` is typed from `en.ts`, so a missing key fails the build.

### 5. Test it

Table-driven config tests are the cheap, high-value part — `Validate` has no I/O,
so they run instantly. For `Check`, use `net.Listen` on `127.0.0.1:0` and point
the config at the resulting port rather than reaching for a real host.

```sh
go test ./internal/monitor/types/echocheck/ -v
go test ./internal/api/ -run Contract      # the registry.ts agreement
```

---

## Contributing a notification provider

### The interface

```go
type Notifier interface {
	Type() string
	// Validate checks a notification's jsonb config (plaintext at this
	// layer — secrets are decrypted at the persistence boundary) and
	// returns a helpful error when it is unusable. It must not perform I/O.
	Validate(cfg json.RawMessage) error
	// Send delivers one event. It must honour ctx (the dispatcher applies
	// the per-send timeout there); a non-nil error marks the delivery as
	// failed in the delivery audit log.
	Send(ctx context.Context, cfg json.RawMessage, event domain.NotificationEvent) error
}
```

The shape mirrors `Checker`, with three differences worth knowing:

- **Secrets arrive in plaintext.** Channel configs are AES-GCM encrypted at the
  persistence boundary and decrypted before they reach you. Do not log the config,
  and do not echo it back in an error message.
- **The dispatcher isolates failures per channel.** Returning an error fails only
  your delivery; other channels still fire. Return one rather than retrying
  internally — resend and escalation are the dispatcher's job, not yours.
- **`Send` may be called concurrently** for different events. Keep the `Notifier`
  itself stateless; build the HTTP client per call or make it safe to share.

### The package

Create `internal/notify/providers/<name>/`, and register it the same way:

```go
func init() { registry.Register(New()) }
```

Then add the blank import to `cmd/statwarden/main.go` alongside the other providers.

Provider forms are also declared in `web/src/lib/api/registry.ts`, in
`notificationTypeSpecs`, under the same contract test. `webhook_url` vs
`webhookUrl` is exactly the drift that test exists to catch.

### Testing without a network

Every existing provider is tested against an `httptest.Server`; copy that
pattern rather than mocking the HTTP client. `internal/notify/fake` gives you a
`Notifier` for tests that only need the dispatcher to have something to call.

---

## Checklist

- [ ] Package under `internal/monitor/types/<name>check/` or `internal/notify/providers/<name>/`
- [ ] `init()` calls `registry.Register`
- [ ] Blank import added to `cmd/statwarden/main.go`
- [ ] `Validate` does no I/O and rejects unknown fields
- [ ] `Check` / `Send` honours `ctx`
- [ ] Field spec added to `web/src/lib/api/registry.ts` with keys matching the `json` tags
- [ ] Type added to the table in `internal/api/registry_contract_test.go`
- [ ] Label keys in **both** i18n catalogues
- [ ] `go test ./...` and `cd web && npm run check` green
