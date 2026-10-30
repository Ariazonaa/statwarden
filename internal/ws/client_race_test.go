package ws

import (
	"sync"
	"testing"
)

// clientCount exposes the connected-client count (test-only).
func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// serverClient returns the single server-side *client the hub registered for a
// freshly dialled connection.
func serverClient(t *testing.T, h *Hub) *client {
	t.Helper()
	waitFor(t, func() bool { return h.clientCount() == 1 })
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		return c
	}
	t.Fatal("no server-side client registered")
	return nil
}

// TestConcurrentSendAndCloseNoPanic reproduces the "send on closed channel"
// panic that used to be possible when the connection is torn down (writePump
// exit / unregister) while readPump's trySend and the hub's broadcast are still
// writing to c.send. Many senders race close() on every iteration.
//
// With the fix c.send is never closed — teardown is signalled via c.done — so
// no send can panic. Reverting the fix (restoring close(c.send) in unregister
// and dropping the c.done arms) makes this test crash the binary with "send on
// closed channel", as verified during development. Runs clean under -race.
func TestConcurrentSendAndCloseNoPanic(t *testing.T) {
	for iter := 0; iter < 300; iter++ {
		h := testHub()
		conn, srv := dialTest(t, h, true)
		c := serverClient(t, h)

		// Subscribe the client so the hub broadcast path also writes to c.send,
		// exercising both send sites (lockless trySend and locked broadcast).
		if reason := h.subscribe(c, "monitor:1"); reason != "" {
			t.Fatalf("subscribe: %s", reason)
		}

		var wg sync.WaitGroup
		payload := []byte("x")

		// Lockless senders: the readPump.trySend path.
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 400; j++ {
					c.trySend(payload)
				}
			}()
		}
		// Hub broadcasts: the locked send path.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				h.broadcast("monitor:1", payload)
			}
		}()
		// Tear the connection down mid-flight.
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.close()
		}()

		wg.Wait()
		conn.Close()
		srv.Close()
	}
}
