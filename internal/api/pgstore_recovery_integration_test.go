//go:build integration

package api

import (
	"context"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
)

// recovery_checks (migration 0020) is written by CreateMonitor/UpdateMonitor
// through renumbered placeholder lists, and read back by the scheduler through a
// hand-written query beside the stale sqlc one. MemStore can see none of that: a
// placeholder shifted by one is a silent column swap, not a compile error.
func TestRecoveryChecksRoundTripThroughPostgres(t *testing.T) {
	ctx := context.Background()
	store := newPgStore(t)

	in := MonitorInput{
		Name: "api", Type: "http", Config: []byte(`{"url":"https://example.test"}`),
		Interval: 60, MaxRetries: 2, RetryInterval: 20, RecoveryChecks: 3, Active: true,
		Description: "desc", LinkURL: "https://docs.example.test",
	}
	m, err := store.CreateMonitor(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.RecoveryChecks != 3 || m.MaxRetries != 2 || m.Description != "desc" {
		t.Fatalf("created = %+v, want recovery 3, retries 2 and the description in place", m)
	}

	in.RecoveryChecks = 5
	in.LinkURL = "https://docs2.example.test"
	m, err = store.UpdateMonitor(ctx, m.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if m.RecoveryChecks != 5 || m.LinkURL != "https://docs2.example.test" || !m.Active {
		t.Fatalf("updated = %+v, want recovery 5 with the neighbouring columns intact", m)
	}

	// 0 is "unset" and stored as the floor, never rejected by the CHECK.
	in.RecoveryChecks = 0
	if m, err = store.UpdateMonitor(ctx, m.ID, in); err != nil || m.RecoveryChecks != 1 {
		t.Fatalf("update with 0 = %+v, %v; want stored as 1", m, err)
	}
	in.RecoveryChecks = 4
	if _, err = store.UpdateMonitor(ctx, m.ID, in); err != nil {
		t.Fatal(err)
	}

	// The scheduler's own load.
	active, err := engine.NewPgStore(testPool, nil, nil).ListActiveMonitors(ctx)
	if err != nil {
		t.Fatalf("engine list: %v", err)
	}
	if len(active) != 1 || active[0].RecoveryChecks != 4 || active[0].MaxRetries != 2 {
		t.Fatalf("engine loaded %+v, want one monitor with recovery 4", active)
	}
}

// The column's floor is enforced by the database itself, not only by the API.
func TestRecoveryChecksBelowOneIsRejectedByTheSchema(t *testing.T) {
	newPgStore(t)
	id := insertMonitor(t, "m")
	_, err := testPool.Exec(context.Background(), `UPDATE monitors SET recovery_checks = 0 WHERE id = $1`, id)
	if err == nil {
		t.Fatal("recovery_checks = 0 was accepted by the schema")
	}
	if code, _ := pgErrorCode(t, err); code != "23514" {
		t.Fatalf("SQLSTATE = %s, want 23514 (check_violation)", code)
	}
}
