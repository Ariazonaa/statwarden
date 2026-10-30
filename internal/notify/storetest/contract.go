// Package storetest holds the behavioural contract that every notify.Store
// implementation has to satisfy, as one table run against both of them.
//
// It exists because of a specific, repeated failure. The Go suite runs against
// MemStore; the SQL in PgStore is only executed behind the integration tag. So
// the two could agree on the SHAPE of an answer and differ on its EDGES with
// nothing to notice — which is exactly what happened twice on this branch: a
// heartbeat window that returned limit+1 rows from SQL and limit from Go, and a
// monitor input that satisfied MemStore while violating a CHECK constraint the
// real schema has.
//
// Writing more PgStore-only tests would not fix that, and on a machine with no
// PostgreSQL it means writing tests that cannot be run before they are pushed.
// One shared contract does better on both counts: the assertions are exercised
// locally against MemStore, so a bug in the TEST is found immediately, and what
// CI then validates is the thing it is uniquely able to see — the difference
// between the two implementations.
package storetest

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify"
)

// Fixture is one implementation, ready to test, plus the seams a real database
// needs that an in-memory map does not.
type Fixture struct {
	Store notify.Store
	// Monitor creates a monitor and returns its id. The outbox has a foreign
	// key to it, so a test cannot simply invent an id and expect PostgreSQL to
	// accept it — this is precisely the class of difference the contract is for.
	Monitor func(t *testing.T, name string) int64
	// Channel creates a notification channel and returns its id. Like Monitor,
	// this is a foreign key in the real schema — notification_deliveries
	// references notifications — so an invented id is accepted by a map and
	// rejected by PostgreSQL. Finding that out is the reason the contract runs
	// on both.
	Channel func(t *testing.T, name string) int64
	// ExpireLeases makes every outbox row due again, standing in for the
	// passage of time. Rows are leased on insert, so without it a claim in the
	// same instant correctly finds nothing.
	ExpireLeases func(t *testing.T)
}

// Run executes the contract. newFixture is called once per subtest so each
// starts from a clean store.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()
	t.Run("outbox lease blocks a second claim", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "leased")

		if _, err := f.Store.EnqueueOutbox(ctx, downRecord(t, id), time.Hour); err != nil {
			t.Fatalf("EnqueueOutbox: %v", err)
		}
		// Inserted with a live lease: the writer owns it, so it is not due.
		claimed, err := f.Store.ClaimOutbox(ctx, time.Minute, 10)
		if err != nil {
			t.Fatalf("ClaimOutbox: %v", err)
		}
		if len(claimed) != 0 {
			t.Fatalf("a freshly written row was claimed by someone else: %+v", claimed)
		}
	})

	t.Run("a lapsed lease makes a row due again, with attempts advanced", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "lapsed")

		rowID, err := f.Store.EnqueueOutbox(ctx, downRecord(t, id), 0)
		if err != nil {
			t.Fatalf("EnqueueOutbox: %v", err)
		}
		first := claimOne(t, f, ctx)
		if first.ID != rowID || first.Attempts != 1 {
			t.Fatalf("first claim = %+v, want id %d attempts 1", first, rowID)
		}
		// Still leased.
		if got, cerr := f.Store.ClaimOutbox(ctx, time.Minute, 10); cerr != nil || len(got) != 0 {
			t.Fatalf("a leased row was claimed again: %+v, %v", got, cerr)
		}
		f.ExpireLeases(t)
		second := claimOne(t, f, ctx)
		if second.ID != rowID || second.Attempts != 2 {
			t.Fatalf("second claim = %+v, want id %d attempts 2", second, rowID)
		}
	})

	t.Run("the payload survives the round trip", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "payload")

		ev := domain.StateChange{
			MonitorID: id, MonitorName: "payload",
			From: domain.StatusUp, To: domain.StatusDown,
			At: time.Now().UTC().Truncate(time.Second), Message: "connection refused",
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Store.EnqueueOutbox(ctx, notify.OutboxRecord{
			Kind: notify.OutboxStateChange, MonitorID: id, Payload: payload,
		}, 0); err != nil {
			t.Fatalf("EnqueueOutbox: %v", err)
		}
		got := claimOne(t, f, ctx)
		if got.Kind != notify.OutboxStateChange || got.MonitorID != id {
			t.Fatalf("claimed = %+v, want a state_change for monitor %d", got, id)
		}
		var back domain.StateChange
		if err := json.Unmarshal(got.Payload, &back); err != nil {
			t.Fatalf("payload does not decode: %v", err)
		}
		if back.To != ev.To || back.Message != ev.Message || back.MonitorID != ev.MonitorID {
			t.Fatalf("event changed in transit: %+v", back)
		}
	})

	t.Run("completing removes the row", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "complete")
		rowID, err := f.Store.EnqueueOutbox(ctx, downRecord(t, id), 0)
		if err != nil {
			t.Fatalf("EnqueueOutbox: %v", err)
		}
		if err := f.Store.CompleteOutbox(ctx, rowID); err != nil {
			t.Fatalf("CompleteOutbox: %v", err)
		}
		if got, cerr := f.Store.ClaimOutbox(ctx, time.Minute, 10); cerr != nil || len(got) != 0 {
			t.Fatalf("a completed row is still claimable: %+v, %v", got, cerr)
		}
	})

	t.Run("completing an unknown row is not an error", func(t *testing.T) {
		// The dispatcher calls this after a delivery it may already have
		// completed on a previous attempt; treating that as a failure would
		// turn an idempotent operation into a logged error every sweep.
		f := newFixture(t)
		if err := f.Store.CompleteOutbox(context.Background(), 999999); err != nil {
			t.Fatalf("CompleteOutbox on a missing row = %v, want nil", err)
		}
	})

	t.Run("giving up returns what it deleted", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "exhausted")
		rowID, err := f.Store.EnqueueOutbox(ctx, downRecord(t, id), 0)
		if err != nil {
			t.Fatalf("EnqueueOutbox: %v", err)
		}
		// Three claims, three attempts.
		for range 3 {
			claimOne(t, f, ctx)
			f.ExpireLeases(t)
		}
		// Below the budget nothing is touched.
		if dropped, derr := f.Store.DropExhaustedOutbox(ctx, 4); derr != nil || len(dropped) != 0 {
			t.Fatalf("DropExhaustedOutbox(4) = %+v, %v; want nothing dropped", dropped, derr)
		}
		dropped, err := f.Store.DropExhaustedOutbox(ctx, 3)
		if err != nil {
			t.Fatalf("DropExhaustedOutbox: %v", err)
		}
		if len(dropped) != 1 || dropped[0].ID != rowID || dropped[0].Attempts != 3 {
			t.Fatalf("dropped = %+v, want the exhausted row with its attempt count", dropped)
		}
	})

	t.Run("claim respects the limit and takes the oldest first", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "ordering")
		var ids []int64
		for range 5 {
			rowID, err := f.Store.EnqueueOutbox(ctx, downRecord(t, id), 0)
			if err != nil {
				t.Fatalf("EnqueueOutbox: %v", err)
			}
			ids = append(ids, rowID)
		}
		got, err := f.Store.ClaimOutbox(ctx, time.Minute, 2)
		if err != nil {
			t.Fatalf("ClaimOutbox: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("claimed %d rows, want 2", len(got))
		}
		if got[0].ID != ids[0] || got[1].ID != ids[1] {
			t.Fatalf("claimed %d,%d; want the two oldest %d,%d",
				got[0].ID, got[1].ID, ids[0], ids[1])
		}
	})

	t.Run("awaiting recovery is the channel's last word", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		id := f.Monitor(t, "recovery")
		told := f.Channel(t, "told")
		resolved := f.Channel(t, "resolved")
		failed := f.Channel(t, "failed")

		record(t, f, ctx, told, id, domain.EventKindDown, notify.DeliverySuccess)
		record(t, f, ctx, resolved, id, domain.EventKindDown, notify.DeliverySuccess)
		record(t, f, ctx, resolved, id, domain.EventKindUp, notify.DeliverySuccess)
		// A failed delivery is not a word at all.
		record(t, f, ctx, failed, id, domain.EventKindDown, notify.DeliveryFailed)

		got, err := f.Store.ChannelsAwaitingRecovery(ctx, id)
		if err != nil {
			t.Fatalf("ChannelsAwaitingRecovery: %v", err)
		}
		slices.Sort(got)
		if len(got) != 1 || got[0] != told {
			t.Fatalf("awaiting = %v, want [%d]: %d was already resolved and %d was "+
				"never actually reached", got, told, resolved, failed)
		}
	})

	t.Run("awaiting recovery ignores other monitors", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		mine := f.Monitor(t, "mine")
		other := f.Monitor(t, "other")
		ch := f.Channel(t, "ch")
		record(t, f, ctx, ch, other, domain.EventKindDown, notify.DeliverySuccess)

		got, err := f.Store.ChannelsAwaitingRecovery(ctx, mine)
		if err != nil {
			t.Fatalf("ChannelsAwaitingRecovery: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("awaiting = %v, want none — another monitor's delivery leaked in", got)
		}
	})
}

func downRecord(t *testing.T, monitorID int64) notify.OutboxRecord {
	t.Helper()
	payload, err := json.Marshal(domain.StateChange{
		MonitorID: monitorID, To: domain.StatusDown,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return notify.OutboxRecord{
		Kind: notify.OutboxStateChange, MonitorID: monitorID, Payload: payload,
	}
}

func claimOne(t *testing.T, f Fixture, ctx context.Context) notify.OutboxItem {
	t.Helper()
	got, err := f.Store.ClaimOutbox(ctx, time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimOutbox: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(got))
	}
	return got[0]
}

func record(
	t *testing.T, f Fixture, ctx context.Context,
	notificationID, monitorID int64, kind domain.NotificationEventKind, status notify.DeliveryStatus,
) {
	t.Helper()
	err := f.Store.RecordDelivery(ctx, notify.Delivery{
		NotificationID: notificationID,
		MonitorID:      monitorID,
		ProviderType:   "fake",
		EventKind:      kind,
		Status:         status,
		Attempts:       1,
		StartedAt:      time.Now().UTC(),
		FinishedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
}
