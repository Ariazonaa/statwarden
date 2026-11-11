package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/engine"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/netguard"
)

// With BLOCK_PRIVATE_TARGETS on, a monitor pointed at a private address must
// not reach its checker at all — the engine's pre-flight is what covers the
// monitor types whose client library dials on its own — and the heartbeat has
// to say why, or the operator sees an unexplained outage.
func TestBlockedTargetNeverReachesTheChecker(t *testing.T) {
	netguard.SetEnabled(true)
	t.Cleanup(func() { netguard.SetEnabled(false) })

	reg := registry.New()
	chk := &scriptChecker{typ: "fake", fallback: domain.CheckResult{Status: domain.StatusUp}}
	reg.Register(chk)
	e, store, clk := newEngine(t, engine.Options{Registry: reg})

	m := testMonitor(1, "fake")
	m.Config = json.RawMessage(`{"host":"127.0.0.1","port":6379}`)
	if err := e.Upsert(m); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	hbs := waitHeartbeats(t, store, 1)

	if n := chk.started.Load(); n != 0 {
		t.Fatalf("the checker ran %d time(s) for a blocked target", n)
	}
	hb := hbs[0]
	if hb.Status != domain.StatusDown {
		t.Fatalf("heartbeat status = %s, want down", hb.Status)
	}
	if !strings.Contains(hb.Message, "BLOCK_PRIVATE_TARGETS") {
		t.Fatalf("heartbeat message %q does not say why the check was refused", hb.Message)
	}
}
