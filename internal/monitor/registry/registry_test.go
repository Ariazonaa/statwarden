package registry

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// stubChecker is a minimal Checker for registry tests.
type stubChecker struct{ name string }

func (s stubChecker) Type() string                   { return s.name }
func (s stubChecker) Validate(json.RawMessage) error { return nil }
func (s stubChecker) Check(context.Context, json.RawMessage) (domain.CheckResult, error) {
	return domain.CheckResult{Status: domain.StatusUp}, nil
}

// stubPassive additionally carries the PassiveChecker marker.
type stubPassive struct{ stubChecker }

func (stubPassive) Passive() {}

func TestRegisterGetList(t *testing.T) {
	r := New()
	r.Register(stubChecker{name: "tcp"})
	r.Register(stubChecker{name: "http"})
	r.Register(stubPassive{stubChecker{name: "push"}})

	if _, ok := r.Get("dns"); ok {
		t.Fatal("Get(dns) should miss")
	}
	c, ok := r.Get("http")
	if !ok || c.Type() != "http" {
		t.Fatalf("Get(http) = %v, %v", c, ok)
	}

	got := r.List()
	want := []string{"http", "push", "tcp"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v (sorted)", got, want)
		}
	}
}

func TestPassiveMarker(t *testing.T) {
	r := New()
	r.Register(stubChecker{name: "http"})
	r.Register(stubPassive{stubChecker{name: "push"}})

	active, _ := r.Get("http")
	if _, isPassive := active.(PassiveChecker); isPassive {
		t.Fatal("http must not be passive")
	}
	passive, _ := r.Get("push")
	if _, isPassive := passive.(PassiveChecker); !isPassive {
		t.Fatal("push must be passive")
	}
}

func TestRegisterPanics(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(r *Registry)
		reg     Checker
	}{
		{
			name: "duplicate type",
			prepare: func(r *Registry) {
				r.Register(stubChecker{name: "http"})
			},
			reg: stubChecker{name: "http"},
		},
		{
			name: "empty type name",
			reg:  stubChecker{name: ""},
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
	r.Register(stubChecker{name: "seed"})
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
