package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// pagePasswordReq submits a page password from a given peer address.
func (h *harness) pagePasswordReq(ctx context.Context, slug, password, peer string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/public/status/"+slug, nil)
	req.Header.Set("X-Page-Password", password)
	req.RemoteAddr = peer
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// TestPagePasswordLockout pins that repeated wrong passwords from one address
// lock that address out of that page before any further argon2 run. Every
// attempt costs 64 MiB of memory-hard hashing on an anonymous endpoint, and
// without a lockout the only brake was the generic public rate limit.
func TestPagePasswordLockout(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{Slug: "vault", Title: "Vault", Published: &pub, Password: "hunter2"})
	h.createPage(admin, statusPageRequest{Slug: "other", Title: "Other", Published: &pub, Password: "hunter2"})

	const attacker = "198.51.100.7:4000"
	for i := range lockoutThreshold {
		if rec := h.pagePasswordReq(t.Context(), "vault", "wrong", attacker); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	// Locked: even the right password is refused, without being checked.
	rec := h.pagePasswordReq(t.Context(), "vault", "hunter2", attacker)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password while locked = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	// The bucket is (page, address): another visitor, and another page for the
	// same address, are unaffected — a stranger cannot lock everyone out.
	if rec := h.pagePasswordReq(t.Context(), "vault", "hunter2", "203.0.113.9:4000"); rec.Code != http.StatusOK {
		t.Errorf("other address = %d, want 200", rec.Code)
	}
	if rec := h.pagePasswordReq(t.Context(), "other", "hunter2", attacker); rec.Code != http.StatusOK {
		t.Errorf("other page from the locked address = %d, want 200", rec.Code)
	}
}

// TestPagePasswordConcurrencyIsBounded pins that password checks run through a
// fixed number of slots. Sixty parallel anonymous attempts used to mean sixty
// concurrent 64 MiB argon2 runs; a request that cannot get a slot in time is
// turned away instead of queuing more memory behind the others.
func TestPagePasswordConcurrencyIsBounded(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{Slug: "busy", Title: "Busy", Published: &pub, Password: "hunter2"})

	// Occupy every slot, as that many in-flight checks would.
	g := h.srv.pagePasswordGuard()
	for range cap(g.slots) {
		g.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	rec := h.pagePasswordReq(ctx, "busy", "hunter2", "198.51.100.8:4000")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("check with every slot taken = %d, want 503", rec.Code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %v for a slot past the request's own deadline", time.Since(start))
	}

	// Freed slots serve again.
	for range cap(g.slots) {
		<-g.slots
	}
	if rec := h.pagePasswordReq(t.Context(), "busy", "hunter2", "198.51.100.8:4000"); rec.Code != http.StatusOK {
		t.Fatalf("check after slots freed = %d, want 200", rec.Code)
	}
}
