package registry

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// stubNotifier is a minimal Notifier for registry tests.
type stubNotifier struct{ name string }

func (s stubNotifier) Type() string                   { return s.name }
func (s stubNotifier) Validate(json.RawMessage) error { return nil }
func (s stubNotifier) Send(context.Context, json.RawMessage, domain.NotificationEvent) error {
	return nil
}

func TestRegisterGetList(t *testing.T) {
	r := New()
	r.Register(stubNotifier{name: "webhook"})
	r.Register(stubNotifier{name: "smtp"})
	r.Register(stubNotifier{name: "slack"})

	if _, ok := r.Get("telegram"); ok {
		t.Fatal("Get(telegram) should miss")
	}
	n, ok := r.Get("smtp")
	if !ok || n.Type() != "smtp" {
		t.Fatalf("Get(smtp) = %v, %v", n, ok)
	}

	got := r.List()
	want := []string{"slack", "smtp", "webhook"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v (sorted)", got, want)
		}
	}
}

func TestRegisterPanics(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(r *Registry)
		reg     Notifier
	}{
		{
			name: "duplicate type",
			prepare: func(r *Registry) {
				r.Register(stubNotifier{name: "webhook"})
			},
			reg: stubNotifier{name: "webhook"},
		},
		{
			name: "empty type name",
			reg:  stubNotifier{name: ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			if tt.prepare != nil {
				tt.prepare(r)
			}
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			r.Register(tt.reg)
		})
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := New()
	r.Register(stubNotifier{name: "seed"})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				r.Get("seed")
				r.List()
			}
		}()
	}
	wg.Wait()
}
