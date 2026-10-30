package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// --- Invariant 1: at least one active admin must remain -------------------

// TestLastActiveAdminCannotBeDemoted pins that the sole active admin cannot
// strip their own admin role: the update leaves the instance with no
// administrator, so it must be rejected. Without the guard in handleUpdateUser
// the plain UpdateUser applies the demotion and locks everyone out of
// administration.
func TestLastActiveAdminCannotBeDemoted(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/users/%d", admin.userID),
		updateUserRequest{Email: "admin@example.com", Role: "user"}, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("demote last admin = %d, want 409; body %s", rec.Code, rec.Body.String())
	}

	// The store must be untouched: the admin is still an admin.
	u, err := h.store.GetUserByID(context.Background(), admin.userID)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	if u.Role != auth.RoleAdmin || !u.IsActive {
		t.Fatalf("admin after rejected demotion = role %q active %v, want admin/true", u.Role, u.IsActive)
	}
}

// TestLastActiveAdminCannotBeDeactivated pins the same invariant against the
// is_active path: deactivating the only admin would leave no active admin, so
// it is rejected.
func TestLastActiveAdminCannotBeDeactivated(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	inactive := false
	rec := h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/users/%d", admin.userID),
		updateUserRequest{Email: "admin@example.com", Role: "admin", IsActive: &inactive}, admin)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deactivate last admin = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	u, err := h.store.GetUserByID(context.Background(), admin.userID)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	if !u.IsActive {
		t.Fatalf("admin deactivated despite being the last one")
	}
}

// TestAdminGuardAllowsDemotionWhenAnotherAdminRemains guards against the
// invariant being too aggressive: with two admins, demoting one is fine — it is
// only the step that would remove the LAST active admin that must fail.
func TestAdminGuardAllowsDemotionWhenAnotherAdminRemains(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	second := h.createUser(admin, "admin2@example.com", "admin")

	// Two admins now exist: demoting the first must succeed.
	rec := h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/users/%d", admin.userID),
		updateUserRequest{Email: "admin@example.com", Role: "user"}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote one of two admins = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// Only the second admin remains; demoting it too must now be rejected. The
	// second admin drives the request (the first is no longer an admin).
	rec = h.requestWith(http.MethodPut, fmt.Sprintf("/api/v1/users/%d", second.userID),
		updateUserRequest{Email: "admin2@example.com", Role: "user"}, second)
	if rec.Code != http.StatusConflict {
		t.Fatalf("demote last remaining admin = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteUserGuardRejectsRemovingLastAdmin exercises the store-level delete
// guard directly. The HTTP path also blocks self-delete, but the store
// invariant must hold on its own: deleting the only active admin is rejected,
// while deleting one of two admins is allowed.
func TestDeleteUserGuardRejectsRemovingLastAdmin(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()

	admin, err := store.CreateInitialAdmin(ctx, "admin@example.com", "admin", "hash", "Admin")
	if err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}

	if err := store.DeleteUserGuarded(ctx, admin.ID); err != ErrLastAdmin {
		t.Fatalf("delete sole admin = %v, want ErrLastAdmin", err)
	}
	if _, err := store.GetUserByID(ctx, admin.ID); err != nil {
		t.Fatalf("sole admin was deleted despite the guard: %v", err)
	}

	// With a second admin present, deleting the first is fine.
	second, err := store.CreateUser(ctx, "admin2@example.com", "admin2", "hash", "Admin2", "admin")
	if err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	if err := store.DeleteUserGuarded(ctx, admin.ID); err != nil {
		t.Fatalf("delete one of two admins = %v, want nil", err)
	}
	// And now the survivor is the last admin: deleting it is rejected again.
	if err := store.DeleteUserGuarded(ctx, second.ID); err != ErrLastAdmin {
		t.Fatalf("delete last remaining admin = %v, want ErrLastAdmin", err)
	}
}

// --- Invariant 2: first-admin bootstrap is atomic -------------------------

// TestSetupIsAtomicUnderConcurrency pins that concurrent first-run setup
// requests cannot each create an admin. Without the serialised check-and-insert
// (CreateInitialAdmin) every goroutine sees CountUsers==0 — the window is wide
// because each spends tens of milliseconds hashing its password between the
// check and the insert — and, using distinct e-mails so the unique-email
// constraint does not mask the race, they all succeed, leaving several admins.
// With the fix exactly one wins and the rest get 409.
func TestSetupIsAtomicUnderConcurrency(t *testing.T) {
	h := newHarness(t)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // fire all requests as close together as possible
			rec := h.request(http.MethodPost, "/api/v1/setup", setupRequest{
				Email:       fmt.Sprintf("admin%d@example.com", i),
				Password:    "supersecret",
				DisplayName: "Admin",
			})
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected setup status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("concurrent setup created %d admins, want exactly 1 (codes: %v)", created, codes)
	}
	if conflicts != n-1 {
		t.Fatalf("concurrent setup returned %d conflicts, want %d (codes: %v)", conflicts, n-1, codes)
	}

	total, err := h.store.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("count users: %v", err)
	}
	if total != 1 {
		t.Fatalf("after concurrent setup there are %d users, want 1", total)
	}
}
