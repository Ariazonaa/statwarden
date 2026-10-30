//go:build integration

// Live-PostgreSQL tests for the two hand-written queries introduced with the
// outage-recovery work. Both were previously exercised by nothing at all: the
// fast suite runs entirely against MemStore, which cannot see a placeholder /
// argument mismatch, a wrong column name or a join that silently matches
// nothing. Their placeholders were counted by hand, and that is exactly the
// kind of verification this file replaces.
//
// notify.OpenOutages lives in another package, but it is exercised here rather
// than in a second integration harness: this package already provisions a
// throwaway database and applies the real migrations, and api already imports
// notify, so borrowing the schema costs nothing and duplicating TestMain would.
//
// See pgstore_integration_test.go for how the database is provisioned.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify"
	"github.com/Ariazonaa/statwarden/internal/retention"
)

// insertMonitorStatus creates a monitor with an explicit status and active flag.
func insertMonitorStatus(t *testing.T, name, status string, active bool) int64 {
	t.Helper()
	var id int64
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO monitors (name, type, status, active) VALUES ($1, 'http', $2, $3) RETURNING id`,
		name, status, active).Scan(&id)
	if err != nil {
		t.Fatalf("insert monitor %q: %v", name, err)
	}
	return id
}

// insertHeartbeatRow writes one heartbeat directly, so a test can place a
// transition at an exact instant.
func insertHeartbeatRow(t *testing.T, monitorID int64, at time.Time, status string, important bool) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO heartbeats (monitor_id, checked_at, status, important)
		 VALUES ($1, $2, $3, $4)`, monitorID, at, status, important)
	if err != nil {
		t.Fatalf("insert heartbeat: %v", err)
	}
}

// TestPgOpenOutages runs the query behind Dispatcher.Restore against a real
// schema. Restore is what stops a restart mid-outage from swallowing the
// recovery alert, so a query that silently returns nothing would reinstate
// exactly the bug it was written to fix — and would do it invisibly.
func TestPgOpenOutages(t *testing.T) {
	newPgStore(t) // truncates
	ctx := context.Background()
	store := notify.NewPgStore(testPool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	downSince := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	broken := insertMonitorStatus(t, "broken", "down", true)
	// The transition INTO down, plus a later non-transition beat that must not
	// be mistaken for the start of the outage.
	insertHeartbeatRow(t, broken, downSince, "down", true)
	insertHeartbeatRow(t, broken, downSince.Add(time.Hour), "down", false)

	// None of these may appear.
	healthy := insertMonitorStatus(t, "healthy", "up", true)
	insertHeartbeatRow(t, healthy, downSince, "up", true)
	insertMonitorStatus(t, "paused-and-down", "down", false)
	insertMonitorStatus(t, "pending", "pending", true)

	outages, err := store.OpenOutages(ctx)
	if err != nil {
		t.Fatalf("OpenOutages: %v", err)
	}
	if len(outages) != 1 {
		t.Fatalf("outages = %+v, want exactly the active DOWN monitor", outages)
	}
	if outages[0].MonitorID != broken {
		t.Fatalf("monitor id = %d, want %d", outages[0].MonitorID, broken)
	}
	// The outage must date from the TRANSITION, not from the newest beat: the
	// reported duration is what the recovery message tells the operator.
	if got := outages[0].Since.UTC().Truncate(time.Second); !got.Equal(downSince) {
		t.Fatalf("since = %s, want the important down beat at %s", got, downSince)
	}
}

// A monitor whose transition heartbeat has already been pruned by retention
// must still be reported — with a fallback start, not dropped. Losing it would
// mean losing its recovery alert too.
func TestPgOpenOutagesWithoutATransitionBeat(t *testing.T) {
	newPgStore(t)
	before := time.Now().UTC()
	id := insertMonitorStatus(t, "pruned", "down", true)

	outages, err := notify.NewPgStore(testPool, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		OpenOutages(context.Background())
	if err != nil {
		t.Fatalf("OpenOutages: %v", err)
	}
	if len(outages) != 1 || outages[0].MonitorID != id {
		t.Fatalf("outages = %+v, want the monitor with no heartbeat history", outages)
	}
	if outages[0].Since.Before(before.Add(-time.Minute)) {
		t.Fatalf("since = %s, want the now() fallback", outages[0].Since)
	}
}

// TestPgStatusPageSlugsForMonitor runs the lookup behind the live status-page
// poke. It only ever returned data through a three-table join, so a wrong
// column or a stale placeholder would read as "this monitor is on no page" —
// indistinguishable from the bug where the resolver was never wired at all.
func TestPgStatusPageSlugsForMonitor(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()

	shown := insertMonitor(t, "shown")
	other := insertMonitor(t, "other")

	mustPage := func(slug string, published bool, monitors ...int64) {
		t.Helper()
		p := StatusPage{Slug: slug, Title: slug, Theme: "auto", Published: published, MonitorIDs: monitors}
		if _, err := s.CreateStatusPage(ctx, p); err != nil {
			t.Fatalf("create page %q: %v", slug, err)
		}
	}
	mustPage("public-a", true, shown)
	mustPage("public-b", true, shown, other)
	mustPage("draft", false, shown)    // unpublished: answers 404 publicly
	mustPage("elsewhere", true, other) // does not carry `shown`

	slugs, err := s.StatusPageSlugsForMonitor(ctx, shown)
	if err != nil {
		t.Fatalf("StatusPageSlugsForMonitor: %v", err)
	}
	want := []string{"public-a", "public-b"} // ORDER BY slug
	if len(slugs) != len(want) {
		t.Fatalf("slugs = %v, want %v", slugs, want)
	}
	for i, w := range want {
		if slugs[i] != w {
			t.Fatalf("slugs = %v, want %v", slugs, want)
		}
	}

	// A monitor on no published page yields an empty slice, never an error.
	orphan := insertMonitor(t, "orphan")
	got, err := s.StatusPageSlugsForMonitor(ctx, orphan)
	if err != nil {
		t.Fatalf("StatusPageSlugsForMonitor(orphan): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("slugs for an unlisted monitor = %v, want none", got)
	}
}

// TestPgListHeartbeatWindowStridesTheWindow runs the third hand-written query
// added by this work — the one whose stride is computed in SQL. Getting the
// modulo wrong there would silently return the newest slice of the window
// again, which is the exact bug it replaced.
func TestPgListHeartbeatWindowStridesTheWindow(t *testing.T) {
	s := newPgStore(t)
	ctx := context.Background()
	id := insertMonitor(t, "dense")

	now := time.Now().UTC().Truncate(time.Second)
	oldest := now.Add(-200 * time.Minute)
	for i := range 200 {
		insertHeartbeatRow(t, id, oldest.Add(time.Duration(i)*time.Minute), "up", false)
	}

	beats, err := s.ListHeartbeatWindow(ctx, id, oldest, now, 20, false)
	if err != nil {
		t.Fatalf("ListHeartbeatWindow: %v", err)
	}
	if len(beats) == 0 || len(beats) > 20 {
		t.Fatalf("returned %d beats, want 1..20", len(beats))
	}
	// The decisive property: the sample spans the window rather than clustering
	// at its newest end.
	oldestReturned := beats[len(beats)-1].CheckedAt.UTC()
	if covered := now.Sub(oldestReturned); covered < 150*time.Minute {
		t.Fatalf("the sample covers only %v of a 200-minute window; it was taken from the newest end", covered)
	}
	// Both ends exactly, not merely "most of the way". This query exists twice —
	// as SQL here and as Go in MemStore — and the Go suite only ever exercises
	// the second, so the two can agree on the shape of the answer and differ on
	// its edges without anything noticing. Pinning the endpoints in both is what
	// keeps them the same function.
	if got := beats[0].CheckedAt.UTC(); !got.Equal(oldest.Add(199 * time.Minute)) {
		t.Fatalf("first point = %s, want the newest %s", got, oldest.Add(199*time.Minute))
	}
	if !oldestReturned.Equal(oldest) {
		t.Fatalf("last point = %s, want the oldest %s", oldestReturned, oldest)
	}

	// The same boundaries the MemStore table covers, so a divergence between the
	// two implementations shows up as a failure rather than as a chart that is
	// subtly wrong in production only.
	for _, tc := range []struct{ limit, wantMax int }{{1, 1}, {2, 2}, {7, 7}, {199, 199}, {200, 200}} {
		got, gerr := s.ListHeartbeatWindow(ctx, id, oldest, now, tc.limit, false)
		if gerr != nil {
			t.Fatalf("ListHeartbeatWindow(limit=%d): %v", tc.limit, gerr)
		}
		if len(got) == 0 || len(got) > tc.wantMax {
			t.Fatalf("limit %d returned %d points, want 1..%d", tc.limit, len(got), tc.wantMax)
		}
		if tc.limit >= 2 && !got[len(got)-1].CheckedAt.UTC().Equal(oldest) {
			t.Fatalf("limit %d: last point = %s, want the oldest %s",
				tc.limit, got[len(got)-1].CheckedAt.UTC(), oldest)
		}
	}
	// A window smaller than the limit comes back whole and unsampled.
	all, err := s.ListHeartbeatWindow(ctx, id, oldest, oldest.Add(9*time.Minute), 20, false)
	if err != nil {
		t.Fatalf("ListHeartbeatWindow (narrow): %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("narrow window returned %d beats, want all 10", len(all))
	}
}

// A deleted monitor takes its pending alerts with it: there is nothing left to
// notify anyone about.
//
// The lease, the attempt counting, the payload round trip and giving up all
// moved into the shared contract (TestPgStoreSatisfiesTheContract), which runs
// the same assertions against MemStore where they can be executed before being
// pushed. This one stays because a foreign-key cascade has no in-memory
// counterpart to compare against.
func TestPgOutboxCascadesWithTheMonitor(t *testing.T) {
	newPgStore(t)
	ctx := context.Background()
	store := notify.NewPgStore(testPool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	monitorID := insertMonitor(t, "doomed")
	if _, err := store.EnqueueOutbox(ctx, outboxRec(t, monitorID), 0); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM monitors WHERE id = $1`, monitorID); err != nil {
		t.Fatalf("delete monitor: %v", err)
	}
	var remaining int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("outbox holds %d rows for a deleted monitor, want 0", remaining)
	}
}

// outboxRec is a minimal DOWN record for tests that only care about the row.
func outboxRec(t *testing.T, monitorID int64) notify.OutboxRecord {
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

// insertDeliveryRow writes one delivery-log row at an exact instant.
func insertDeliveryRow(
	t *testing.T, notificationID, monitorID *int64, kind, status string, finishedAt time.Time,
) int64 {
	t.Helper()
	var id int64
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO notification_deliveries
		     (notification_id, monitor_id, provider_type, event_kind, status, started_at, finished_at)
		 VALUES ($1, $2, 'fake', $3, $4, $5, $5) RETURNING id`,
		notificationID, monitorID, kind, status, finishedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	return id
}

// TestPgPruneDeliveriesKeepsOpenOutages pins that retention cannot delete the rows
// the recovery lookup depends on.
//
// The delivery log is not only an audit trail. ChannelsAwaitingRecovery reads it
// to answer "who was told this monitor is down and not yet told otherwise", and
// Dispatcher.Restore asks that once per open outage after a restart. Prune the
// DOWN delivery and the answer becomes "nobody", so the recovery for that outage
// is never announced — the same silence the outbox and the restore path exist to
// remove. It needs an outage longer than the retention window, which the 365-day
// default makes rare and a 7-day window makes ordinary.
//
// Live, because the guard is a NOT EXISTS in hand-written SQL: MemStore has no
// notion of a monitor's status, so the fast suite cannot see this at all.
func TestPgPruneDeliveriesKeepsOpenOutages(t *testing.T) {
	newPgStore(t) // truncates monitors (CASCADE reaches the delivery log)
	ctx := context.Background()
	pruner := retention.NewPgStore(testPool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	notifyStore := notify.NewPgStore(testPool, nil, log)

	channel := insertNotificationRow(t, "pager")
	long := time.Now().UTC().Add(-90 * 24 * time.Hour) // far older than the cutoff

	// Still down: its DOWN delivery is the one Restore needs.
	broken := insertMonitorStatus(t, "broken", "down", true)
	keep := insertDeliveryRow(t, &channel, &broken, "down", "success", long)

	// Recovered long ago: nothing is owed, so its rows are ordinary history.
	healed := insertMonitorStatus(t, "healed", "up", true)
	dropUp := insertDeliveryRow(t, &channel, &healed, "down", "success", long)

	// Paused while down. OpenOutages skips it (status 'down' AND active), so
	// Restore will never ask about it and the guard must not protect it either —
	// the two predicates have to agree.
	paused := insertMonitorStatus(t, "paused-and-down", "down", false)
	dropPaused := insertDeliveryRow(t, &channel, &paused, "down", "success", long)

	// A test send carries no monitor at all.
	dropTest := insertDeliveryRow(t, &channel, nil, "test", "success", long)

	deleted, err := pruner.PruneDeliveriesBefore(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("PruneDeliveriesBefore: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3 (recovered, paused-and-down, test send)", deleted)
	}

	for _, id := range []int64{dropUp, dropPaused, dropTest} {
		if deliveryExists(t, id) {
			t.Errorf("delivery %d survived the prune but nothing depends on it", id)
		}
	}
	if !deliveryExists(t, keep) {
		t.Fatal("the DOWN delivery of a monitor that is STILL down was pruned; " +
			"its recovery alert can no longer be addressed to anyone")
	}

	// The lookup Restore actually performs still finds the channel.
	told, err := notifyStore.ChannelsAwaitingRecovery(ctx, broken)
	if err != nil {
		t.Fatalf("ChannelsAwaitingRecovery: %v", err)
	}
	if len(told) != 1 || told[0] != channel {
		t.Fatalf("channels awaiting recovery = %v, want [%d] — retention broke the "+
			"recovery lookup", told, channel)
	}
}

// deliveryExists reports whether a delivery row is still present.
func deliveryExists(t *testing.T, id int64) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_deliveries WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count delivery %d: %v", id, err)
	}
	return n > 0
}
