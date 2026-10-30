// Package registry defines the Notifier plugin interface and the provider
// registry. Each notification provider lives in its own subpackage under
// internal/notify/providers and self-registers via init(); the dispatcher and
// API only ever see the type-name strings stored on notification rows.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Notifier is the plugin interface every notification provider implements.
// Adding a new channel never touches core code: implement Notifier, call
// Register in init(), blank-import the package from cmd/statwarden.
type Notifier interface {
	// Type returns the unique registry name stored in notifications.type.
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

// Registry is a thread-safe map of type name to Notifier.
type Registry struct {
	mu        sync.RWMutex
	notifiers map[string]Notifier
}

// New returns an empty registry. Most code uses the package-level Default
// registry; New exists for tests.
func New() *Registry {
	return &Registry{notifiers: make(map[string]Notifier)}
}

// Register adds n under n.Type(). It panics on an empty type name or a
// duplicate registration — both are programmer errors that must fail at
// init() time, not at first use.
func (r *Registry) Register(n Notifier) {
	name := n.Type()
	if name == "" {
		panic("notify/registry: Register with empty type name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.notifiers[name]; dup {
		panic(fmt.Sprintf("notify/registry: duplicate registration for type %q", name))
	}
	r.notifiers[name] = n
}

// Get returns the notifier for a type name.
func (r *Registry) Get(name string) (Notifier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.notifiers[name]
	return n, ok
}

// List returns all registered type names, sorted.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.notifiers))
	for name := range r.notifiers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Default is the process-wide registry that init() self-registration in the
// provider subpackages populates.
var Default = New()

// Register adds n to the Default registry.
func Register(n Notifier) { Default.Register(n) }

// Get looks up a type in the Default registry.
func Get(name string) (Notifier, bool) { return Default.Get(name) }
