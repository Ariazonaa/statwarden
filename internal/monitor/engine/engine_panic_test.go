package engine_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/testutil"
)

// armPanicClock is a Clock that panics on the Nth AfterFunc.
//
// The seam matters: runCheck re-arms the job's timer through Clock.AfterFunc
// while holding Engine.mu, so panicking there is a panic raised INSIDE the
// engine's critical section — which is the only way to reach the state this
// test is about.
type armPanicClock struct {
	*fakeClock
	arms    atomic.Int64
	panicOn int64
}

func (c *armPanicClock) AfterFunc(d time.Duration, f func()) engine.Timer {
	if c.arms.Add(1) == c.panicOn {
		panic("clock exploded while the engine held its lock")
	}
	return c.fakeClock.AfterFunc(d, f)
}

// TestPanicUnderTheLockDoesNotWedgeTheEngine pins that a panic raised while
// Engine.mu is held leaves the mutex released.
//
// runCheck's backstop recovers such a panic and calls clearRunning, which takes
// Engine.mu. A Go mutex is not reentrant, so if the panic unwinds without
// releasing the lock, that recover blocks forever — holding the engine's single
// mutex. Every other monitor then blocks on it too: one panic in one job stops
// ALL monitoring, permanently and silently, which is precisely the outcome the
// backstop exists to prevent.
//
// The assertion is deliberately about the engine still ANSWERING rather than
// about the panic itself: Scheduling() takes the same mutex, so a call that
// cannot return is the deadlock, and one that returns proves the unwind
// released the lock.
func TestPanicUnderTheLockDoesNotWedgeTheEngine(t *testing.T) {
	chk := &scriptChecker{typ: "fake"}
	reg := registry.New()
	reg.Register(chk)

	// Arm 1 is Upsert scheduling the monitor; arm 2 is runCheck re-arming it
	// after the check, which happens under the lock.
	clk := &armPanicClock{fakeClock: newFakeClock(), panicOn: 2}

	e := engine.New(engine.Options{
		Registry:         reg,
		Store:            engine.NewMemStore(),
		Clock:            clk,
		Workers:          1,
		MaxStartupJitter: -1,
	})
	// Stop() takes Engine.mu too, so on a REGRESSION the cleanup would block as
	// well and hang the whole package until go test's global timeout kills it,
	// burying the failure under a goroutine dump. Bounded here so a regression
	// fails this one test and leaves the suite readable.
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			ctx, cancel := context.WithTimeout(context.Background(), testutil.Wait(2*time.Second))
			defer cancel()
			_ = e.Stop(ctx)
		}()
		select {
		case <-stopped:
		case <-time.After(testutil.Wait(3 * time.Second)):
		}
	})

	if err := e.Upsert(testMonitor(1, "fake")); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckNow(1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the check to run", func() bool { return chk.started.Load() >= 1 })
	waitFor(t, "the re-arm to panic", func() bool { return clk.arms.Load() >= 2 })

	// Scheduling() needs Engine.mu. Off the test goroutine, so a deadlock fails
	// this test instead of hanging the whole package until the -timeout kills it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Scheduling()
	}()
	select {
	case <-done:
	case <-time.After(testutil.Wait(3 * time.Second)):
		t.Fatal("Engine.mu was still held after a panic unwound through it — " +
			"the recover in runCheck cannot re-acquire it, so the engine is wedged " +
			"and every monitor has stopped being checked")
	}
}
