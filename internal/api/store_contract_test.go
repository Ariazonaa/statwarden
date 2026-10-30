package api

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/domain"
)

// The behavioural contract for api.Store, as one table run against both
// implementations: MemStore here, PgStore behind the integration tag in
// pgstore_contract_integration_test.go.
//
// The Go suite only ever executes MemStore, so the two can agree on the shape
// of an answer and differ on its edges with nothing to notice. That is not
// hypothetical on this branch: ListHeartbeatWindow returned limit+1 rows from
// SQL and limit from Go, and a MonitorInput that satisfied a map violated a
// CHECK constraint the real schema has.
//
// Running the same assertions against MemStore is what makes the PgStore side
// worth anything — a mistake in the TEST is found on a machine with no
// database, so a failure over there means the implementations differ.

// storeFixture is one implementation plus the seams a database needs that a
// map does not.
type storeFixture struct {
	Store Store
	// Heartbeat records one beat. It is not on the Store interface — the
	// engine writes heartbeats through its own store — so each side supplies
	// its own way of getting a row in.
	Heartbeat func(t *testing.T, monitorID int64, at time.Time, status string)
}

func runStoreContract(t *testing.T, newFixture func(t *testing.T) storeFixture) {
	t.Helper()
	ctx := context.Background()

	newUser := func(t *testing.T, f storeFixture, email string) User {
		t.Helper()
		u, err := f.Store.CreateUser(ctx, email, email, "hash-"+email, email, "admin")
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
		return u
	}
	newSession := func(t *testing.T, f storeFixture, userID int64, id string) {
		t.Helper()
		now := time.Now().UTC()
		err := f.Store.CreateSession(ctx, Session{
			ID: id, UserID: userID, CreatedAt: now,
			ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
		})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}
	newMonitor := func(t *testing.T, f storeFixture, name string) int64 {
		t.Helper()
		m, err := f.Store.CreateMonitor(ctx, MonitorInput{
			Name: name, Type: "http", Interval: 60, RetryInterval: 60, Active: true,
			Config: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("CreateMonitor(%s): %v", name, err)
		}
		return m.ID
	}

	t.Run("changing a password revokes the other sessions and keeps one", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "keep@example.com")
		newSession(t, f, u.ID, "keep-me")
		newSession(t, f, u.ID, "kill-me")

		if err := f.Store.ChangePassword(ctx, u.ID, "new-hash", "keep-me"); err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		if _, err := f.Store.GetSession(ctx, "keep-me"); err != nil {
			t.Fatalf("the caller's own session was revoked: %v", err)
		}
		if _, err := f.Store.GetSession(ctx, "kill-me"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the other session survived: %v", err)
		}
		creds, err := f.Store.GetCredentialsByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetCredentialsByID: %v", err)
		}
		if creds.PasswordHash != "new-hash" {
			t.Fatalf("password hash = %q, want the new one", creds.PasswordHash)
		}
	})

	t.Run("changing a password invalidates outstanding reset tokens", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "reset@example.com")
		if _, err := f.Store.CreatePasswordReset(ctx, PasswordReset{
			UserID: u.ID, TokenHash: auth.HashToken("tok"),
			ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreatePasswordReset: %v", err)
		}
		if err := f.Store.ChangePassword(ctx, u.ID, "new-hash", ""); err != nil {
			t.Fatalf("ChangePassword: %v", err)
		}
		// The token must no longer be redeemable: a mail sitting in an
		// intercepted inbox outliving the change made to lock somebody out is
		// the sequence this prevents.
		_, err := f.Store.ConsumePasswordReset(ctx, auth.HashToken("tok"), "attacker", time.Now().UTC())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("the token was still redeemable: %v", err)
		}
	})

	t.Run("a reset token can only be redeemed once", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "once@example.com")
		if _, err := f.Store.CreatePasswordReset(ctx, PasswordReset{
			UserID: u.ID, TokenHash: auth.HashToken("single"),
			ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreatePasswordReset: %v", err)
		}
		if _, err := f.Store.ConsumePasswordReset(ctx,
			auth.HashToken("single"), "first", time.Now().UTC()); err != nil {
			t.Fatalf("first redemption: %v", err)
		}
		if _, err := f.Store.ConsumePasswordReset(ctx,
			auth.HashToken("single"), "second", time.Now().UTC()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second redemption = %v, want ErrNotFound", err)
		}
	})

	t.Run("an expired reset token is refused", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "expired@example.com")
		if _, err := f.Store.CreatePasswordReset(ctx, PasswordReset{
			UserID: u.ID, TokenHash: auth.HashToken("old"),
			ExpiresAt: time.Now().UTC().Add(-time.Minute), CreatedAt: time.Now().UTC().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("CreatePasswordReset: %v", err)
		}
		if _, err := f.Store.ConsumePasswordReset(ctx,
			auth.HashToken("old"), "hash", time.Now().UTC()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an expired token was redeemed: %v", err)
		}
	})

	t.Run("revoking API keys touches only that account", func(t *testing.T) {
		f := newFixture(t)
		mine := newUser(t, f, "mine@example.com")
		other := newUser(t, f, "other@example.com")
		for _, u := range []User{mine, other} {
			if _, err := f.Store.CreateAPIKey(ctx, APIKey{
				UserID: u.ID, Name: "k", Prefix: "upt_" + u.Email[:3],
				KeyHash: "hash-" + u.Email, Scopes: []string{ScopeRead},
			}); err != nil {
				t.Fatalf("CreateAPIKey: %v", err)
			}
		}
		n, err := f.Store.DeleteAPIKeysByUser(ctx, mine.ID)
		if err != nil {
			t.Fatalf("DeleteAPIKeysByUser: %v", err)
		}
		if n != 1 {
			t.Fatalf("revoked %d keys, want 1", n)
		}
		if keys, kerr := f.Store.ListAPIKeys(ctx, mine.ID); kerr != nil || len(keys) != 0 {
			t.Fatalf("the account still holds %+v (%v)", keys, kerr)
		}
		if keys, kerr := f.Store.ListAPIKeys(ctx, other.ID); kerr != nil || len(keys) != 1 {
			t.Fatalf("another account's keys were revoked too: %+v (%v)", keys, kerr)
		}
	})

	t.Run("revoking keys for an account with none is not an error", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "nokeys@example.com")
		n, err := f.Store.DeleteAPIKeysByUser(ctx, u.ID)
		if err != nil || n != 0 {
			t.Fatalf("DeleteAPIKeysByUser = %d, %v; want 0, nil", n, err)
		}
	})

	t.Run("provisioning leaves no user behind when the address is taken", func(t *testing.T) {
		f := newFixture(t)
		newUser(t, f, "taken@example.com")

		_, err := f.Store.CreateUserWithIdentity(ctx, "taken@example.com", "taken2",
			"Taken", "viewer", ExternalIdentity{Provider: "sso", Issuer: "i", Subject: "s"})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("CreateUserWithIdentity on a taken address = %v, want ErrConflict", err)
		}
		// And the identity did not attach itself to anything.
		if _, gerr := f.Store.GetIdentity(ctx, "i", "s"); !errors.Is(gerr, ErrNotFound) {
			t.Fatalf("an identity was created for a user that does not exist: %v", gerr)
		}
	})

	t.Run("provisioning writes the user and the identity together", func(t *testing.T) {
		f := newFixture(t)
		u, err := f.Store.CreateUserWithIdentity(ctx, "sso@example.com", "sso",
			"SSO", "viewer", ExternalIdentity{Provider: "sso", Issuer: "i", Subject: "s"})
		if err != nil {
			t.Fatalf("CreateUserWithIdentity: %v", err)
		}
		id, err := f.Store.GetIdentity(ctx, "i", "s")
		if err != nil {
			t.Fatalf("GetIdentity: %v", err)
		}
		if id.UserID != u.ID {
			t.Fatalf("identity belongs to %d, want the new user %d", id.UserID, u.ID)
		}
		// No local password: the empty hash is the canonical "sign-in is via
		// the provider" marker, not a weak password.
		creds, err := f.Store.GetCredentialsByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetCredentialsByID: %v", err)
		}
		if creds.PasswordHash != "" {
			t.Fatalf("a provisioned account has a password hash %q", creds.PasswordHash)
		}
	})

	t.Run("a monitor cannot become its own ancestor", func(t *testing.T) {
		f := newFixture(t)
		a := newMonitor(t, f, "a")
		b := newMonitor(t, f, "b")

		base := MonitorInput{Type: "http", Interval: 60, RetryInterval: 60, Active: true, Config: []byte(`{}`)}
		in := base
		in.Name, in.ParentID = "b", &a
		if _, err := f.Store.UpdateMonitor(ctx, b, in); err != nil {
			t.Fatalf("b.parent = a: %v", err)
		}
		// a.parent = b closes the loop.
		in = base
		in.Name, in.ParentID = "a", &b
		if _, err := f.Store.UpdateMonitor(ctx, a, in); !errors.Is(err, ErrParentCycle) {
			t.Fatalf("closing the loop = %v, want ErrParentCycle", err)
		}
		in = base
		in.Name, in.ParentID = "a", &a
		if _, err := f.Store.UpdateMonitor(ctx, a, in); !errors.Is(err, ErrParentCycle) {
			t.Fatalf("self-parent = %v, want ErrParentCycle", err)
		}
	})

	t.Run("the heartbeat window keeps both ends and honours the limit", func(t *testing.T) {
		f := newFixture(t)
		id := newMonitor(t, f, "dense")
		base := time.Now().UTC().Add(-300 * time.Minute).Truncate(time.Second)
		const rows = 200
		for i := range rows {
			f.Heartbeat(t, id, base.Add(time.Duration(i)*time.Minute), "up")
		}
		newest := base.Add((rows - 1) * time.Minute)

		// The boundaries the stride arithmetic actually has. Getting any of
		// them wrong is invisible in a chart and obvious here.
		for _, limit := range []int{1, 2, 7, 20, 199, 200, 500} {
			got, err := f.Store.ListHeartbeatWindow(ctx, id, base, newest, limit, false)
			if err != nil {
				t.Fatalf("ListHeartbeatWindow(limit=%d): %v", limit, err)
			}
			if len(got) == 0 {
				t.Fatalf("limit %d returned nothing", limit)
			}
			if len(got) > limit {
				t.Fatalf("limit %d returned %d points", limit, len(got))
			}
			if !got[0].CheckedAt.UTC().Equal(newest) {
				t.Fatalf("limit %d: first point = %s, want the newest %s",
					limit, got[0].CheckedAt.UTC(), newest)
			}
			if limit >= 2 && !got[len(got)-1].CheckedAt.UTC().Equal(base) {
				t.Fatalf("limit %d: last point = %s, want the oldest %s — the window "+
					"does not reach its own start", limit, got[len(got)-1].CheckedAt.UTC(), base)
			}
		}
	})

	t.Run("a window narrower than the limit comes back whole", func(t *testing.T) {
		f := newFixture(t)
		id := newMonitor(t, f, "sparse")
		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		for i := range 5 {
			f.Heartbeat(t, id, base.Add(time.Duration(i)*time.Minute), "up")
		}
		got, err := f.Store.ListHeartbeatWindow(ctx, id, base, base.Add(4*time.Minute), 20, false)
		if err != nil {
			t.Fatalf("ListHeartbeatWindow: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("returned %d of 5 beats — a window under the limit must not be sampled", len(got))
		}
	})

	t.Run("the TOTP re-seal reads and writes only the secret", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "totp@example.com")
		if err := f.Store.SetUserTOTP(ctx, u.ID, "PLAINSECRET", true, []string{"code-hash"}); err != nil {
			t.Fatalf("SetUserTOTP: %v", err)
		}
		stored, err := f.Store.StoredTOTPSecrets(ctx)
		if err != nil {
			t.Fatalf("StoredTOTPSecrets: %v", err)
		}
		if stored[u.ID] != "PLAINSECRET" {
			t.Fatalf("StoredTOTPSecrets[%d] = %q, want the stored secret", u.ID, stored[u.ID])
		}
		if err := f.Store.UpdateTOTPSecret(ctx, u.ID, "SEALED"); err != nil {
			t.Fatalf("UpdateTOTPSecret: %v", err)
		}
		creds, err := f.Store.GetCredentialsByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetCredentialsByID: %v", err)
		}
		if creds.TOTPSecret != "SEALED" {
			t.Fatalf("secret = %q, want the new one", creds.TOTPSecret)
		}
		// The backfill must not disturb what it is not fixing: an account left
		// with 2FA off, or without its backup codes, is worse than a plaintext
		// secret.
		if !creds.TOTPEnabled {
			t.Fatal("re-sealing turned 2FA off")
		}
		if len(creds.BackupCodes) != 1 || creds.BackupCodes[0] != "code-hash" {
			t.Fatalf("backup codes = %v, want them untouched", creds.BackupCodes)
		}
	})

	t.Run("accounts without a TOTP secret are not listed", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "nototp@example.com")
		stored, err := f.Store.StoredTOTPSecrets(ctx)
		if err != nil {
			t.Fatalf("StoredTOTPSecrets: %v", err)
		}
		if _, ok := stored[u.ID]; ok {
			t.Fatalf("an account with no secret was listed for re-sealing")
		}
	})

	// Every list method answers an empty LIST, never a null.
	//
	// The two implementations disagreed on all but one of these: MemStore built
	// its slices with make/literals, PgStore declared `var out []T` and returned
	// nil when the query matched nothing. json.Marshal turns that into `null`,
	// and the handlers pass the slice straight through — so against PostgreSQL an
	// instance with no tags answered `null` where openapi.json declares
	// `type: array` with no `nullable`, breaking any generated or strict client.
	// The whole Go suite runs MemStore, so nothing saw it; the frontend only
	// coped because every wire type is typed `WireTag[] | null`, which
	// accommodates the bug rather than the contract.
	//
	// ListMonitorGroups already did it correctly, which is what makes the other
	// fifteen oversights rather than a decision.
	t.Run("list methods return an empty list, never nil", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "lists@example.com")
		// Theme is set explicitly: the real schema CHECKs it and a map does not,
		// which is the divergence this pairing exists to surface. The handler
		// defaults an unknown theme to "auto", so no product path sends "".
		page, err := f.Store.CreateStatusPage(ctx, StatusPage{
			Slug: "empty", Title: "Empty", Theme: "auto",
		})
		if err != nil {
			t.Fatalf("CreateStatusPage: %v", err)
		}
		mon := newMonitor(t, f, "listing")

		// name -> the call, each against a store holding nothing of that kind.
		checks := map[string]func() (any, error){
			"ListTags":             func() (any, error) { return f.Store.ListTags(ctx) },
			"ListUsers":            func() (any, error) { return f.Store.ListUsers(ctx) },
			"ListSettings":         func() (any, error) { return f.Store.ListSettings(ctx) },
			"ListNotifications":    func() (any, error) { return f.Store.ListNotifications(ctx) },
			"ListMaintenance":      func() (any, error) { return f.Store.ListMaintenance(ctx) },
			"ListStatusPages":      func() (any, error) { return f.Store.ListStatusPages(ctx) },
			"ListMonitorGroups":    func() (any, error) { return f.Store.ListMonitorGroups(ctx) },
			"ListIncidents":        func() (any, error) { return f.Store.ListIncidents(ctx, page.ID) },
			"ListSubscribers":      func() (any, error) { return f.Store.ListSubscribers(ctx, page.ID, false) },
			"ListAPIKeys":          func() (any, error) { return f.Store.ListAPIKeys(ctx, u.ID) },
			"ListSessionsByUser":   func() (any, error) { return f.Store.ListSessionsByUser(ctx, u.ID) },
			"ListIdentitiesByUser": func() (any, error) { return f.Store.ListIdentitiesByUser(ctx, u.ID) },
			"ListHeartbeats":       func() (any, error) { return f.Store.ListHeartbeats(ctx, mon, 10, false) },
			"ListHeartbeatWindow": func() (any, error) {
				now := time.Now().UTC()
				return f.Store.ListHeartbeatWindow(ctx, mon, now.Add(-time.Hour), now, 10, false)
			},
		}
		for name, call := range checks {
			got, err := call()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if reflect.ValueOf(got).IsNil() {
				t.Errorf("%s returned a nil slice; it marshals as JSON null, and "+
					"openapi.json declares an array", name)
			}
		}
	})

	// The sibling of the check above, one level down.
	//
	// That one asserts the RETURNED LIST is not nil. It says nothing about the
	// collections INSIDE each record, and emptySliceAsArray cannot help there
	// either — it only rewrites a slice that IS the whole response body, not one
	// nested in a struct. So a record whose own collection is nil marshals to
	// `"monitor_ids": null` while openapi.json declares a non-nullable array.
	//
	// The empty case is not an edge here, it is the DEFAULT one: a maintenance
	// window that names no monitor applies to every monitor (see
	// internal/maintenance.Window), which is how an operator schedules
	// fleet-wide downtime.
	// The recycle bin is where the two stores are most likely to drift, because
	// they hide a binned monitor by completely different mechanisms: PgStore adds
	// `deleted_at IS NULL` to a dozen hand-written statements, MemStore moves the
	// row into a second map. One forgotten filter over there is invisible here —
	// which is exactly what this table is for.
	t.Run("a binned monitor disappears from every read and comes back paused", func(t *testing.T) {
		f := newFixture(t)
		id := newMonitor(t, f, "binned")

		if err := f.Store.DeleteMonitor(ctx, id); err != nil {
			t.Fatalf("DeleteMonitor: %v", err)
		}

		if _, err := f.Store.GetMonitor(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetMonitor after delete = %v, want ErrNotFound", err)
		}
		list, total, err := f.Store.ListMonitors(ctx, MonitorFilter{}, Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListMonitors: %v", err)
		}
		for _, m := range list {
			if m.ID == id {
				t.Error("a binned monitor is still in ListMonitors")
			}
		}
		// The count is a SEPARATE statement from the page in PgStore, so a filter
		// applied to one and not the other is a real and silent possibility.
		if total != len(list) {
			t.Errorf("ListMonitors total = %d but returned %d rows; the count and the "+
				"page disagree about the bin", total, len(list))
		}

		// Narrow writes must refuse it too — each has its own statement.
		if err := f.Store.SetMonitorActive(ctx, id, true); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetMonitorActive on a binned monitor = %v, want ErrNotFound", err)
		}
		if err := f.Store.SetMonitorGroup(ctx, id, nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetMonitorGroup on a binned monitor = %v, want ErrNotFound", err)
		}
		if err := f.Store.SetMonitorSchedule(ctx, id, MonitorSchedule{Interval: 30}); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetMonitorSchedule on a binned monitor = %v, want ErrNotFound", err)
		}
		if err := f.Store.AddMonitorTags(ctx, id, nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("AddMonitorTags on a binned monitor = %v, want ErrNotFound", err)
		}
		if err := f.Store.DeleteMonitor(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleting an already-binned monitor = %v, want ErrNotFound", err)
		}

		// The bin itself sees it, and it is paused there.
		binned, err := f.Store.GetDeletedMonitor(ctx, id)
		if err != nil {
			t.Fatalf("GetDeletedMonitor: %v", err)
		}
		if binned.Active {
			t.Error("a binned monitor is still active; the scheduler selects on that column")
		}
		bin, err := f.Store.ListDeletedMonitors(ctx)
		if err != nil {
			t.Fatalf("ListDeletedMonitors: %v", err)
		}
		if len(bin) != 1 || bin[0].ID != id {
			t.Fatalf("bin = %+v, want just the binned monitor", bin)
		}

		restored, err := f.Store.RestoreMonitor(ctx, id)
		if err != nil {
			t.Fatalf("RestoreMonitor: %v", err)
		}
		if restored.Active {
			t.Error("a restored monitor is active; neither store can know it was, so it comes back paused")
		}
		if _, err := f.Store.GetMonitor(ctx, id); err != nil {
			t.Errorf("GetMonitor after restore: %v", err)
		}
		// And the two sets are disjoint again.
		if _, err := f.Store.GetDeletedMonitor(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetDeletedMonitor after restore = %v, want ErrNotFound", err)
		}
	})

	// Purge is the only irreversible path, and it is reachable only through the
	// bin. A store that let it touch a live monitor would hand back the one-click
	// destruction the bin was added to remove.
	// The schema's ON DELETE SET NULL does not know the recycle bin exists: it
	// clears group_id on every row referencing the folder, soft-deleted ones
	// included. MemStore hides binned monitors in a second map, so it has to be
	// told — and a restored monitor pointing at a folder that no longer exists in
	// one implementation and not the other is exactly the drift this file exists
	// to catch.
	t.Run("deleting a folder unfiles the monitors in the bin too", func(t *testing.T) {
		f := newFixture(t)
		g, err := f.Store.CreateMonitorGroup(ctx, "folder", 0)
		if err != nil {
			t.Fatalf("CreateMonitorGroup: %v", err)
		}
		id := newMonitor(t, f, "filed")
		if err := f.Store.SetMonitorGroup(ctx, id, &g.ID); err != nil {
			t.Fatalf("SetMonitorGroup: %v", err)
		}
		if err := f.Store.DeleteMonitor(ctx, id); err != nil {
			t.Fatalf("DeleteMonitor: %v", err)
		}

		if err := f.Store.DeleteMonitorGroup(ctx, g.ID); err != nil {
			t.Fatalf("DeleteMonitorGroup: %v", err)
		}

		restored, err := f.Store.RestoreMonitor(ctx, id)
		if err != nil {
			t.Fatalf("RestoreMonitor: %v", err)
		}
		if restored.GroupID != nil {
			t.Fatalf("the restored monitor still points at folder %d, which was deleted "+
				"while it was in the bin", *restored.GroupID)
		}
	})

	t.Run("purge refuses a live monitor and destroys a binned one", func(t *testing.T) {
		f := newFixture(t)
		id := newMonitor(t, f, "doomed")

		if err := f.Store.PurgeMonitor(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PurgeMonitor on a LIVE monitor = %v, want ErrNotFound", err)
		}
		if _, err := f.Store.GetMonitor(ctx, id); err != nil {
			t.Fatalf("the live monitor did not survive a refused purge: %v", err)
		}

		if err := f.Store.DeleteMonitor(ctx, id); err != nil {
			t.Fatalf("DeleteMonitor: %v", err)
		}
		if err := f.Store.PurgeMonitor(ctx, id); err != nil {
			t.Fatalf("PurgeMonitor: %v", err)
		}
		if _, err := f.Store.GetDeletedMonitor(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("the monitor is still in the bin after a purge")
		}
		if _, err := f.Store.RestoreMonitor(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("a purged monitor could still be restored")
		}
	})

	// The schema cascades a monitor delete across five other tables. MemStore
	// has to do the same by hand, and nothing in the Go suite would notice if it
	// did not — this test is the only thing that would.
	t.Run("purging a monitor takes every reference to it with it", func(t *testing.T) {
		f := newFixture(t)
		id := newMonitor(t, f, "referenced")
		keep := newMonitor(t, f, "bystander")

		child, err := f.Store.CreateMonitor(ctx, MonitorInput{
			Name: "child", Type: "http", Interval: 60, RetryInterval: 60, Active: true,
			Config: []byte(`{}`), ParentID: &id,
		})
		if err != nil {
			t.Fatalf("CreateMonitor(child): %v", err)
		}
		page, err := f.Store.CreateStatusPage(ctx, StatusPage{
			Slug: "cascade", Title: "Cascade", Theme: "auto", Published: true,
			Monitors: []StatusPageMonitorEntry{
				{MonitorID: id, ShowUptime: true},
				{MonitorID: keep, ShowUptime: true},
			},
		})
		if err != nil {
			t.Fatalf("CreateStatusPage: %v", err)
		}
		from := time.Now().UTC().Add(time.Hour)
		to := from.Add(time.Hour)
		win, err := f.Store.CreateMaintenance(ctx, MaintenanceWindow{
			// Strategy and Timezone are spelled out because the INSERT NAMES both
			// columns, so their DEFAULTs never apply and a zero value is written
			// verbatim — "" then fails maintenance_windows_strategy_check. MemStore
			// validates neither, so this omission was invisible until the
			// integration suite ran it against a real PostgreSQL. The handler
			// cannot produce it (toWindow defaults an empty strategy to "single"
			// and Validate rejects anything else); this is the test reaching the
			// store directly, which is the entire point of a contract suite.
			Title:      "planned",
			Strategy:   "single",
			Timezone:   "UTC",
			StartsAt:   &from,
			EndsAt:     &to,
			MonitorIDs: []int64{id, keep},
		})
		if err != nil {
			t.Fatalf("CreateMaintenance: %v", err)
		}
		if _, err := f.Store.EnsureBadgeToken(ctx, id, "tok-cascade"); err != nil {
			t.Fatalf("EnsureBadgeToken: %v", err)
		}

		if err := f.Store.DeleteMonitor(ctx, id); err != nil {
			t.Fatalf("DeleteMonitor: %v", err)
		}
		if err := f.Store.PurgeMonitor(ctx, id); err != nil {
			t.Fatalf("PurgeMonitor: %v", err)
		}

		// A placement pointing at a monitor that no longer exists is exactly the
		// dangling reference the public status page renders as a blank
		// component.
		gotPage, err := f.Store.GetStatusPage(ctx, page.ID)
		if err != nil {
			t.Fatalf("GetStatusPage: %v", err)
		}
		for _, e := range gotPage.Monitors {
			if e.MonitorID == id {
				t.Error("the status page still places the purged monitor")
			}
		}
		if len(gotPage.Monitors) != 1 || gotPage.Monitors[0].MonitorID != keep {
			t.Errorf("page placements = %+v, want only the bystander", gotPage.Monitors)
		}
		for _, mid := range gotPage.MonitorIDs {
			if mid == id {
				t.Error("the flat MonitorIDs projection still names the purged monitor")
			}
		}

		gotWin, err := f.Store.GetMaintenance(ctx, win.ID)
		if err != nil {
			t.Fatalf("GetMaintenance: %v", err)
		}
		for _, mid := range gotWin.MonitorIDs {
			if mid == id {
				t.Error("the maintenance window still covers the purged monitor")
			}
		}
		if len(gotWin.MonitorIDs) != 1 {
			t.Errorf("maintenance monitors = %v, want only the bystander", gotWin.MonitorIDs)
		}

		if _, err := f.Store.MonitorIDByBadgeToken(ctx, "tok-cascade"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the badge token still resolves after the purge: %v", err)
		}

		// parent_id is SET NULL, not CASCADE: a purge must not take the children
		// grouped under it.
		gotChild, err := f.Store.GetMonitor(ctx, child.ID)
		if err != nil {
			t.Fatalf("the child monitor did not survive its parent's purge: %v", err)
		}
		if gotChild.ParentID != nil {
			t.Errorf("the child still points at the purged parent (%d)", *gotChild.ParentID)
		}
	})

	// The bin's ORDER is part of its contract — it is a list somebody scans for
	// the thing they just deleted — and it was only ever asserted with one row in
	// it, which any implementation satisfies.
	t.Run("the bin is ordered most recently deleted first", func(t *testing.T) {
		f := newFixture(t)
		ids := []int64{
			newMonitor(t, f, "first"),
			newMonitor(t, f, "second"),
			newMonitor(t, f, "third"),
		}
		for _, id := range ids {
			if err := f.Store.DeleteMonitor(ctx, id); err != nil {
				t.Fatalf("DeleteMonitor(%d): %v", id, err)
			}
		}

		bin, err := f.Store.ListDeletedMonitors(ctx)
		if err != nil {
			t.Fatalf("ListDeletedMonitors: %v", err)
		}
		if len(bin) != len(ids) {
			t.Fatalf("bin holds %d monitors, want %d", len(bin), len(ids))
		}
		// Newest first. Three deletes in the same millisecond are the ordinary
		// case for a bulk action, so the id is the tie-break on both sides and
		// the expected order is the reverse of the delete order either way.
		for i, want := range []int64{ids[2], ids[1], ids[0]} {
			if bin[i].ID != want {
				t.Errorf("bin[%d] = %d, want %d (most recently deleted first): got %v",
					i, bin[i].ID, want, binIDs(bin))
			}
		}
	})

	// Deactivating through the full-replace UPDATE has to move `status` too.
	//
	// It was the only writer of `active` that did not, and handleUpdateMonitor
	// removes the monitor from the engine straight afterwards — so nothing ever
	// rewrote the column again and a monitor paused this way went on reading
	// "up" on the dashboard, the badge and the public status page. Both stores
	// had the same omission, which is why nothing caught it.
	t.Run("deactivating through an update moves the status too", func(t *testing.T) {
		f := newFixture(t)
		id := newMonitor(t, f, "toggle")

		live, err := f.Store.GetMonitor(ctx, id)
		if err != nil {
			t.Fatalf("GetMonitor: %v", err)
		}
		if live.Status == domain.StatusPaused {
			t.Fatal("a freshly created active monitor is already paused")
		}

		if _, err := f.Store.UpdateMonitor(ctx, id, MonitorInput{
			Name: "toggle", Type: "http", Interval: 60, RetryInterval: 60,
			Active: false, Config: []byte(`{}`),
		}); err != nil {
			t.Fatalf("UpdateMonitor: %v", err)
		}
		paused, err := f.Store.GetMonitor(ctx, id)
		if err != nil {
			t.Fatalf("GetMonitor: %v", err)
		}
		if paused.Active {
			t.Fatal("the update did not deactivate the monitor")
		}
		if paused.Status != domain.StatusPaused {
			t.Errorf("status = %q after deactivating through an update, want paused: "+
				"nothing rewrites this column afterwards, so it stays wrong forever",
				paused.Status)
		}
	})

	t.Run("the bin is empty, never nil", func(t *testing.T) {
		f := newFixture(t)
		bin, err := f.Store.ListDeletedMonitors(ctx)
		if err != nil {
			t.Fatalf("ListDeletedMonitors: %v", err)
		}
		if bin == nil {
			t.Error("ListDeletedMonitors returned nil; it marshals as JSON null and the " +
				"client reads .length on it")
		}
	})

	// The audit export is a FILE someone files as evidence, so its order and its
	// bounds are part of the contract, not an implementation detail. The two
	// stores got this wrong in exactly the way this file's header describes: the
	// map returned insertion order and the SQL returned created_at order, and
	// nothing in the Go suite could see the difference until it was asserted here.
	t.Run("the audit export is newest-first and inclusive at both bounds", func(t *testing.T) {
		f := newFixture(t)
		base := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
		// Written OUT of timestamp order on purpose: insertion order is the wrong
		// answer, so a store that simply reverses its slice fails here.
		for _, at := range []time.Time{base.Add(2 * time.Hour), base, base.Add(time.Hour)} {
			if _, err := f.Store.InsertAudit(ctx, AuditEntry{
				Action: "seed", CreatedAt: at,
			}); err != nil {
				t.Fatalf("InsertAudit(%s): %v", at, err)
			}
		}

		all, err := f.Store.ExportAudit(ctx, time.Time{}, base.Add(24*time.Hour), 10)
		if err != nil {
			t.Fatalf("ExportAudit: %v", err)
		}
		if len(all) != 3 {
			t.Fatalf("ExportAudit returned %d rows, want 3", len(all))
		}
		for i := 1; i < len(all); i++ {
			if all[i].CreatedAt.After(all[i-1].CreatedAt) {
				t.Fatalf("row %d (%s) is newer than the one above it (%s); not newest-first",
					i, all[i].CreatedAt, all[i-1].CreatedAt)
			}
		}

		// Both bounds are inclusive, so a window pinned exactly to the middle entry
		// contains exactly that entry.
		mid, err := f.Store.ExportAudit(ctx, base.Add(time.Hour), base.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("ExportAudit(narrow): %v", err)
		}
		if len(mid) != 1 || !mid[0].CreatedAt.Equal(base.Add(time.Hour)) {
			t.Fatalf("a window pinned to one timestamp returned %d rows: %+v", len(mid), mid)
		}

		// The cap applies AFTER the ordering, so a truncated export keeps the
		// NEWEST rows rather than whichever ones were written first.
		capped, err := f.Store.ExportAudit(ctx, time.Time{}, base.Add(24*time.Hour), 1)
		if err != nil {
			t.Fatalf("ExportAudit(limit 1): %v", err)
		}
		if len(capped) != 1 {
			t.Fatalf("limit 1 returned %d rows", len(capped))
		}
		if !capped[0].CreatedAt.Equal(base.Add(2 * time.Hour)) {
			t.Fatalf("limit 1 kept %s, want the newest row %s",
				capped[0].CreatedAt, base.Add(2*time.Hour))
		}
	})

	t.Run("InsertAudit reports the row it stored", func(t *testing.T) {
		f := newFixture(t)
		// The id is what lets the SIEM forwarder emit an event a collector can
		// deduplicate and join against the CSV export; before this returned the
		// stored row, every forwarded event carried id 0.
		stored, err := f.Store.InsertAudit(ctx, AuditEntry{
			Action: "tag.create", EntityType: "tag", CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("InsertAudit: %v", err)
		}
		if stored.ID == 0 {
			t.Error("InsertAudit returned no row id")
		}
		if stored.CreatedAt.IsZero() {
			t.Error("InsertAudit returned no timestamp")
		}
	})

	// The postmortem columns are server-owned, exactly like status_pages.logo_path:
	// a full-replace incident update must not be able to erase a retrospective the
	// client never loaded. PgStore enforces that by not naming the columns in its
	// UPDATE and MemStore by not copying them off the input struct — two different
	// mechanisms for one rule, which is the definition of something to pin here.
	t.Run("updating an incident leaves its postmortem alone", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "pm@example.com")
		// Theme spelled out, not left zero: status_pages.theme is NOT NULL with a
		// CHECK, so an empty string violates the real schema and only the PgStore
		// run would ever notice.
		page, err := f.Store.CreateStatusPage(ctx, StatusPage{Slug: "pm", Title: "PM", Theme: "auto"})
		if err != nil {
			t.Fatalf("CreateStatusPage: %v", err)
		}
		inc, err := f.Store.CreateIncident(ctx, Incident{
			StatusPageID: page.ID, Title: "outage", Body: "b",
			Severity: "major", Status: "resolved", CreatedBy: &u.ID,
		})
		if err != nil {
			t.Fatalf("CreateIncident: %v", err)
		}

		published := time.Date(2026, 3, 14, 9, 10, 0, 0, time.UTC)
		withPM, err := f.Store.SetIncidentPostmortem(ctx, inc.ID, "the write-up", &published)
		if err != nil {
			t.Fatalf("SetIncidentPostmortem: %v", err)
		}
		if withPM.Postmortem != "the write-up" || withPM.PostmortemPublishedAt == nil {
			t.Fatalf("SetIncidentPostmortem did not report what it stored: %+v", withPM)
		}

		// A stale editor's PUT: every editable field, no postmortem anywhere.
		stale := inc
		stale.Title = "outage (renamed)"
		updated, err := f.Store.UpdateIncident(ctx, inc.ID, stale)
		if err != nil {
			t.Fatalf("UpdateIncident: %v", err)
		}
		if updated.Postmortem != "the write-up" {
			t.Errorf("UpdateIncident erased the postmortem: %q", updated.Postmortem)
		}
		if updated.PostmortemPublishedAt == nil {
			t.Error("UpdateIncident unpublished the postmortem")
		}

		// Clearing is its own operation, and it clears both halves.
		cleared, err := f.Store.SetIncidentPostmortem(ctx, inc.ID, "", nil)
		if err != nil {
			t.Fatalf("SetIncidentPostmortem(clear): %v", err)
		}
		if cleared.Postmortem != "" || cleared.PostmortemPublishedAt != nil {
			t.Errorf("clearing left something behind: %+v", cleared)
		}
	})

	t.Run("collections inside a record are empty, never nil", func(t *testing.T) {
		f := newFixture(t)
		u := newUser(t, f, "nested@example.com")

		// A window with no monitor scoping, i.e. one that applies to all of them.
		start := time.Now().UTC()
		end := start.Add(time.Hour)
		win, err := f.Store.CreateMaintenance(ctx, MaintenanceWindow{
			Title: "fleet-wide", Active: true, Strategy: "single", Timezone: "UTC",
			StartsAt: &start, EndsAt: &end,
		})
		if err != nil {
			t.Fatalf("CreateMaintenance: %v", err)
		}
		page, err := f.Store.CreateStatusPage(ctx, StatusPage{Slug: "nested", Title: "Nested", Theme: "auto"})
		if err != nil {
			t.Fatalf("CreateStatusPage: %v", err)
		}
		// An incident naming no specific monitor — the page-wide announcement.
		inc, err := f.Store.CreateIncident(ctx, Incident{
			StatusPageID: page.ID, Title: "wide", Body: "b",
			Severity: "minor", Status: "investigating", CreatedBy: &u.ID,
		})
		if err != nil {
			t.Fatalf("CreateIncident: %v", err)
		}

		list, err := f.Store.ListMaintenance(ctx)
		if err != nil {
			t.Fatalf("ListMaintenance: %v", err)
		}
		got, err := f.Store.GetMaintenance(ctx, win.ID)
		if err != nil {
			t.Fatalf("GetMaintenance: %v", err)
		}
		incList, err := f.Store.ListIncidents(ctx, page.ID)
		if err != nil {
			t.Fatalf("ListIncidents: %v", err)
		}
		gotInc, err := f.Store.GetIncident(ctx, inc.ID)
		if err != nil {
			t.Fatalf("GetIncident: %v", err)
		}
		gotPage, err := f.Store.GetStatusPage(ctx, page.ID)
		if err != nil {
			t.Fatalf("GetStatusPage: %v", err)
		}

		nested := map[string]any{
			"CreateMaintenance().MonitorIDs": win.MonitorIDs,
			"GetMaintenance().MonitorIDs":    got.MonitorIDs,
			"CreateIncident().MonitorIDs":    inc.MonitorIDs,
			"CreateIncident().Updates":       inc.Updates,
			"GetIncident().MonitorIDs":       gotInc.MonitorIDs,
			"GetIncident().Updates":          gotInc.Updates,
			"GetStatusPage().MonitorIDs":     gotPage.MonitorIDs,
		}
		if len(list) > 0 {
			nested["ListMaintenance()[0].MonitorIDs"] = list[0].MonitorIDs
		}
		if len(incList) > 0 {
			nested["ListIncidents()[0].MonitorIDs"] = incList[0].MonitorIDs
			nested["ListIncidents()[0].Updates"] = incList[0].Updates
		}
		for name, v := range nested {
			if reflect.ValueOf(v).IsNil() {
				t.Errorf("%s is nil; it marshals as JSON null, and openapi.json declares an array", name)
			}
		}
	})
}

// TestMemStoreSatisfiesTheStoreContract runs the contract where it can be
// executed before it is pushed. Its counterpart runs the same table against
// PostgreSQL; see pgstore_contract_integration_test.go.
func TestMemStoreSatisfiesTheStoreContract(t *testing.T) {
	runStoreContract(t, func(*testing.T) storeFixture {
		store := NewMemStore()
		return storeFixture{
			Store: store,
			Heartbeat: func(_ *testing.T, monitorID int64, at time.Time, status string) {
				store.AddHeartbeat(HeartbeatView{
					MonitorID: monitorID, CheckedAt: at, Status: status,
				})
			},
		}
	})
}

// binIDs renders a bin listing for a failure message.
func binIDs(bin []domain.Monitor) []int64 {
	out := make([]int64, 0, len(bin))
	for _, m := range bin {
		out = append(out, m.ID)
	}
	return out
}
