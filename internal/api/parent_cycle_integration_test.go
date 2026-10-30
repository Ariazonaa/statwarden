//go:build integration

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestConcurrentParentChangesCannotCreateACycle is the race the API-level
// validation cannot close.
//
// validateMonitorParent walks the parent chain before the write, and
// UpdateMonitor's transaction locks only the monitor being edited. Two requests
// arriving together — A.parent=B and B.parent=A — therefore both validate
// against a graph that has no cycle, and both commit, after which the graph has
// one. It is not cosmetic: the suppression walk that decides "is this monitor
// down only because its parent is" follows these edges, and the walk's own
// depth cap is the only thing standing between a cycle and a spin.
//
// MemStore cannot reproduce this — one mutex serialises everything — so the Go
// suite that every other test runs in is blind to it by construction. This one
// needs the real thing.
func TestConcurrentParentChangesCannotCreateACycle(t *testing.T) {
	store := newPgStore(t)
	ctx := context.Background()

	a := insertMonitor(t, "cycle-a")
	b := insertMonitor(t, "cycle-b")

	// Both writes are issued at once and each is the other's mirror image.
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)
	swap := func(id, parent int64) {
		defer wg.Done()
		<-start
		p := parent
		_, err := store.UpdateMonitor(ctx, id, MonitorInput{
			Name: "cycle", Type: "http", Interval: 60, MaxRetries: 0,
			RetryInterval: 60, ResendInterval: 0, Active: true, ParentID: &p,
		})
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	wg.Add(2)
	go swap(a, b)
	go swap(b, a)
	close(start)
	wg.Wait()

	// Exactly one must win. Both succeeding is the cycle; both failing would
	// mean the lock is refusing legitimate work.
	var ok, cycles int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrParentCycle):
			cycles++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || cycles != 1 {
		t.Fatalf("results = %d ok, %d cycle-rejected; want exactly one of each "+
			"(errs: %v)", ok, cycles, errs)
	}

	// And the stored graph really is acyclic: walking up from either end
	// terminates.
	for _, start := range []int64{a, b} {
		seen := map[int64]bool{}
		cur := start
		for range maxParentChain + 1 {
			if seen[cur] {
				t.Fatalf("the stored parent graph contains a cycle reachable from %d", start)
			}
			seen[cur] = true
			var next *int64
			if err := testPool.QueryRow(ctx,
				`SELECT parent_id FROM monitors WHERE id = $1`, cur).Scan(&next); err != nil {
				t.Fatalf("walk: %v", err)
			}
			if next == nil {
				break
			}
			cur = *next
		}
	}
}

// A monitor cannot be its own parent, and the check that says so is the one in
// the writing transaction — not only the API's pre-flight walk.
func TestSelfParentIsRejectedByTheStore(t *testing.T) {
	store := newPgStore(t)
	id := insertMonitor(t, "self-parent")
	self := id
	_, err := store.UpdateMonitor(context.Background(), id, MonitorInput{
		Name: "self", Type: "http", Interval: 60, MaxRetries: 0,
		RetryInterval: 60, ResendInterval: 0, Active: true, ParentID: &self,
	})
	if !errors.Is(err, ErrParentCycle) {
		t.Fatalf("self-parent = %v, want ErrParentCycle", err)
	}
}
