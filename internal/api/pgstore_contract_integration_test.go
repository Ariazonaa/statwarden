//go:build integration

package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/notify"
	"github.com/Ariazonaa/statwarden/internal/notify/storetest"
)

// TestPgStoreSatisfiesTheContract runs the same table as
// TestMemStoreSatisfiesTheContract, against a real database.
//
// The pairing is the point. The Go suite only ever executes MemStore, so the
// two implementations could agree on the shape of an answer and differ on its
// edges with nothing to notice — which is how a heartbeat window came to return
// limit+1 rows from SQL and limit from Go. One table, both implementations, and
// a failure here means they diverge rather than that the test is wrong, because
// the assertions have already been exercised on the other side.
func TestPgStoreSatisfiesTheContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Fixture {
		newPgStore(t) // truncates; outbox rows and deliveries cascade with the monitors
		store := notify.NewPgStore(testPool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		return storetest.Fixture{
			Store: store,
			// A real monitor row, because the outbox has a foreign key to it.
			// Inventing an id works in a map and is rejected here, which is
			// exactly the class of difference this pairing exists to catch.
			Monitor: func(t *testing.T, name string) int64 {
				return insertMonitor(t, name)
			},
			Channel: insertNotificationRow,
			ExpireLeases: func(t *testing.T) {
				// No clock seam in SQL: move the leases into the past directly.
				if _, err := testPool.Exec(context.Background(),
					`UPDATE notification_outbox SET claimed_until = now() - interval '1 hour'`); err != nil {
					t.Fatalf("expire leases: %v", err)
				}
			},
		}
	})
}

// TestPgStoreSatisfiesTheStoreContract is the PostgreSQL half of
// TestMemStoreSatisfiesTheStoreContract — the same table, the other
// implementation.
//
// Everything it asserts has already been run against MemStore on a machine
// with no database, so a failure here is a divergence between the two rather
// than a mistake in the assertions. That distinction is the whole reason the
// contract is shared: telling those two apart used to cost a CI round trip.
func TestPgStoreSatisfiesTheStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) storeFixture {
		store := newPgStore(t) // truncates users, monitors and everything cascading
		return storeFixture{
			Store: store,
			Heartbeat: func(t *testing.T, monitorID int64, at time.Time, status string) {
				insertHeartbeatRow(t, monitorID, at, status, false)
			},
		}
	})
}

// insertNotificationRow creates a channel so a delivery can reference it.
// notification_deliveries has a foreign key to notifications, so the ids the
// contract records against have to be real ones — a map accepts any number.
func insertNotificationRow(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO notifications (name, type) VALUES ($1, 'fake') RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("insert notification %q: %v", name, err)
	}
	return id
}
