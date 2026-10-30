package safego

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDoContainsAPanicAndReportsIt(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	if !Do(log, "the unit", func() { panic("boom") }) {
		t.Fatal("Do reported no panic")
	}
	out := buf.String()
	for _, want := range []string{"the unit", "boom", "safego_test.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q: %s", want, out)
		}
	}
}

// The stack is the point of logging at all: the recovered value names what
// went wrong and never says where.
func TestDoLogsAStack(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	Do(log, "x", func() { panic("boom") })
	if !strings.Contains(buf.String(), "goroutine") {
		t.Fatalf("no stack in the log: %s", buf.String())
	}
}

func TestDoReturnsFalseWhenNothingHappens(t *testing.T) {
	ran := false
	if Do(discard(), "x", func() { ran = true }) {
		t.Fatal("Do reported a panic for a clean run")
	}
	if !ran {
		t.Fatal("fn was not called")
	}
}

// The reason Do exists is that the caller keeps going. A loop wrapping ONE
// iteration survives a bad one; a loop wrapped as a whole would stop at the
// first, which is the failure mode this package is meant to avoid.
func TestALoopSurvivesOneBadIteration(t *testing.T) {
	done := 0
	for i := range 5 {
		Do(discard(), "iteration", func() {
			if i == 2 {
				panic("the third one is broken")
			}
			done++
		})
	}
	if done != 4 {
		t.Fatalf("completed %d iterations, want 4 — the loop did not survive", done)
	}
}

func TestGoContainsAPanic(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	Go(discard(), "background", func() {
		defer wg.Done()
		panic("boom")
	})
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the goroutine never finished")
	}
}
