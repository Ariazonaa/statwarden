// Package fake is an in-memory notification provider for tests. It records
// every event it is asked to send and can be told to fail or block, so
// dispatcher tests can assert fan-out isolation, retries and timing without a
// network.
package fake

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// Notifier is a controllable fake registry.Notifier.
type Notifier struct {
	name string

	mu       sync.Mutex
	sent     []domain.NotificationEvent
	failWith error
	// block, when non-nil, is received from inside Send so a test can hold
	// a delivery open (to prove one slow channel does not block others).
	block <-chan struct{}
	calls int
}

// New returns a fake notifier registered under the given type name.
func New(name string) *Notifier { return &Notifier{name: name} }

// Type implements registry.Notifier.
func (n *Notifier) Type() string { return n.name }

// Validate implements registry.Notifier (always accepts).
func (*Notifier) Validate(json.RawMessage) error { return nil }

// Send implements registry.Notifier.
func (n *Notifier) Send(ctx context.Context, _ json.RawMessage, event domain.NotificationEvent) error {
	n.mu.Lock()
	n.calls++
	block := n.block
	failWith := n.failWith
	n.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failWith != nil {
		return failWith
	}
	n.mu.Lock()
	n.sent = append(n.sent, event)
	n.mu.Unlock()
	return nil
}

// FailWith makes every subsequent Send return err (nil clears it).
func (n *Notifier) FailWith(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failWith = err
}

// BlockOn makes Send wait until ch is closed (or ctx is cancelled).
func (n *Notifier) BlockOn(ch <-chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.block = ch
}

// Sent returns a copy of the successfully delivered events.
func (n *Notifier) Sent() []domain.NotificationEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]domain.NotificationEvent(nil), n.sent...)
}

// Count returns the number of successful deliveries.
func (n *Notifier) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sent)
}

// Calls returns how many times Send was invoked (including failures).
func (n *Notifier) Calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}
