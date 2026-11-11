package pingcheck

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

func mustCfg(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// withStats returns a Checker whose ping transport yields fixed statistics,
// so tests never touch the network.
func withStats(st stats) *Checker {
	return &Checker{run: func(context.Context, Config) (stats, error) { return st, nil }}
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("ping checker not self-registered")
	}
	if _, passive := c.(registry.PassiveChecker); passive {
		t.Fatal("ping checker must not be passive")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("ping checker must provide a positive default timeout")
	}
}

func TestCheckLossThreshold(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		st         stats
		wantStatus domain.MonitorStatus
	}{
		{"all received", Config{Host: "h"}, stats{PacketsSent: 4, PacketsRecv: 4, PacketLoss: 0, AvgRTT: 2 * time.Millisecond}, domain.StatusUp},
		{"total loss down by default", Config{Host: "h"}, stats{PacketsSent: 4, PacketsRecv: 0, PacketLoss: 100}, domain.StatusDown},
		{"partial loss up under default", Config{Host: "h"}, stats{PacketsSent: 4, PacketsRecv: 3, PacketLoss: 25, AvgRTT: time.Millisecond}, domain.StatusUp},
		{"partial loss over custom threshold", Config{Host: "h", PacketLossThresholdPercent: 20}, stats{PacketsSent: 5, PacketsRecv: 3, PacketLoss: 40, AvgRTT: time.Millisecond}, domain.StatusDown},
		{"partial loss under custom threshold", Config{Host: "h", PacketLossThresholdPercent: 50}, stats{PacketsSent: 5, PacketsRecv: 3, PacketLoss: 40, AvgRTT: time.Millisecond}, domain.StatusUp},
		{"loss equal to threshold stays up", Config{Host: "h", PacketLossThresholdPercent: 40}, stats{PacketsSent: 5, PacketsRecv: 3, PacketLoss: 40, AvgRTT: time.Millisecond}, domain.StatusUp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := withStats(tt.st)
			res, err := ch.Check(context.Background(), mustCfg(t, tt.cfg))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Message, tt.wantStatus)
			}
			if len(res.Extra) == 0 {
				t.Fatal("ping statistics should be attached as Extra")
			}
		})
	}
}

func TestCheckRunError(t *testing.T) {
	ch := &Checker{run: func(context.Context, Config) (stats, error) {
		return stats{}, errors.New("resolve host: no such host")
	}}
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: "nope.invalid"}))
	if err != nil {
		t.Fatalf("run failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("res = %+v, want down with message", res)
	}
}

func TestExtraJSONShape(t *testing.T) {
	ch := withStats(stats{PacketsSent: 4, PacketsRecv: 4, PacketLoss: 0, AvgRTT: 3 * time.Millisecond})
	res, _ := ch.Check(context.Background(), mustCfg(t, Config{Host: "h"}))
	var got map[string]any
	if err := json.Unmarshal(res.Extra, &got); err != nil {
		t.Fatalf("Extra is not valid JSON: %v", err)
	}
	for _, key := range []string{"packets_sent", "packets_recv", "packet_loss_percent", "avg_rtt_ms"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("Extra missing %q: %v", key, got)
		}
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"h","count":-1}`)); err == nil {
		t.Fatal("Check must reject a negative count")
	}
}

// pro-bing sends one echo per Interval (1s by default) and stops at Timeout, so
// with the default four probes and a monitor timeout of 3s the last request was
// never sent or never answered and a healthy host showed 25% loss. The probes
// are spread so that all of them, and the last reply, fit the time available.
func TestProbeIntervalFitsEveryProbeIntoTheTimeout(t *testing.T) {
	tests := []struct {
		count     int
		remaining time.Duration
		want      time.Duration
	}{
		{4, 10 * time.Second, time.Second}, // plenty of time: pro-bing's default
		{4, 3 * time.Second, 600 * time.Millisecond},
		{100, 10 * time.Second, 10 * time.Second / 101},
		{1, 0, time.Second}, // no deadline: default
	}
	for _, tt := range tests {
		got := probeInterval(tt.count, tt.remaining)
		if got != tt.want {
			t.Errorf("probeInterval(%d, %s) = %s, want %s", tt.count, tt.remaining, got, tt.want)
		}
		if tt.remaining > 0 && got*time.Duration(tt.count+1) > tt.remaining {
			t.Errorf("probeInterval(%d, %s) = %s leaves no time for the last reply", tt.count, tt.remaining, got)
		}
	}
}

// The target is resolved under the check's context. pro-bing resolves through
// net.ResolveIPAddr, which takes none, so a hanging resolver held the check
// past its timeout — and past the engine's shutdown.
func TestRunPingResolvesUnderTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runPing(ctx, Config{Host: "host.example.test"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runPing with a cancelled context = %v, want context.Canceled", err)
	}
}
