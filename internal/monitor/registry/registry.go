// Package registry defines the Checker plugin interface and the type
// registry. Each monitor type lives in its own subpackage under
// internal/monitor/types and self-registers via init(); the engine and API
// only ever see the type-name strings stored on monitor rows.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Checker is the plugin interface every monitor type implements. Adding a new
// type never touches core code: implement Checker, call Register in init(),
// blank-import the package from cmd/statwarden.
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
	//
	// # CheckResult.Message is a published field
	//
	// The message is stored on the heartbeat and served by
	// GET /monitors/{id}/heartbeats to every authenticated reader — a viewer
	// and a read-only API key included — and internal/outbound copies it into
	// the state-change webhook payload. A writer, meanwhile, may point a
	// monitor at any address the process can reach.
	//
	// So a message that echoes what the target sent back turns a pass/fail
	// check into a read channel for internal services. The rule is by PURPOSE,
	// not by transport:
	//
	//   - When the value only tests an ASSERTION the operator configured, say
	//     which assertion failed and what it EXPECTED, never what came back.
	//     The expectation is the operator's own text and tells them everything
	//     debugging needs. httpcheck, mysqlcheck, postgrescheck, mqttcheck and
	//     websocketcheck follow this.
	//   - When the value IS the deliverable — an SNMP OID reading, the DNS
	//     records, a Steam server's player count — reporting it is the feature
	//     and it stays. Same for a bounded enum the remote cannot author, such
	//     as a Docker container state or a gRPC serving status.
	//
	// Prose the REMOTE writes is never a deliverable: prefer a numeric status
	// code over an HTTP status line, whose reason phrase the server chooses.
	Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error)
}

// PassiveChecker marks push-style monitor types: Statwarden does not probe the
// target, the target reports in. The scheduler never arms timers for passive
// types; a watchdog marks stale ones DOWN instead. Check is only invoked for
// manual triggers, if at all.
type PassiveChecker interface {
	Checker
	// Passive is a marker method; implementations are empty.
	Passive()
}

// TimeoutDefaulter is optionally implemented by checkers that want a
// type-specific default timeout (used when the monitor does not override it).
type TimeoutDefaulter interface {
	DefaultTimeout() time.Duration
}

// Registry is a thread-safe map of type name to Checker.
type Registry struct {
	mu       sync.RWMutex
	checkers map[string]Checker
}

// New returns an empty registry. Most code uses the package-level Default
// registry; New exists for tests.
func New() *Registry {
	return &Registry{checkers: make(map[string]Checker)}
}

// Register adds c under c.Type(). It panics on an empty type name or a
// duplicate registration — both are programmer errors that must fail at
// init() time, not at first use.
func (r *Registry) Register(c Checker) {
	name := c.Type()
	if name == "" {
		panic("monitor/registry: Register with empty type name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.checkers[name]; dup {
		panic(fmt.Sprintf("monitor/registry: duplicate registration for type %q", name))
	}
	r.checkers[name] = c
}

// Get returns the checker for a type name.
func (r *Registry) Get(name string) (Checker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.checkers[name]
	return c, ok
}

// List returns all registered type names, sorted.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.checkers))
	for name := range r.checkers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Default is the process-wide registry that init() self-registration in the
// type subpackages populates.
var Default = New()

// Register adds c to the Default registry.
func Register(c Checker) { Default.Register(c) }

// Get looks up a type in the Default registry.
func Get(name string) (Checker, bool) { return Default.Get(name) }
