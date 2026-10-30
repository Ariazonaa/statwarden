// Package pingcheck implements the ICMP/UDP ping monitor type: it sends a
// configurable number of echo requests and reports UP unless the measured
// packet loss exceeds a configurable threshold (so partial loss can alert
// before a total outage). To avoid requiring raw-socket privileges it uses an
// unprivileged UDP ping by default. The probe transport is injectable so tests
// exercise the loss-threshold and result-mapping logic without any network.
package pingcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "ping"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// stats is the transport-independent result of a ping run.
type stats struct {
	PacketsSent int           `json:"packets_sent"`
	PacketsRecv int           `json:"packets_recv"`
	PacketLoss  float64       `json:"packet_loss_percent"`
	AvgRTT      time.Duration `json:"-"`
	AvgRTTMs    float64       `json:"avg_rtt_ms"`
}

// runFunc executes a ping run for a config; injected in tests.
type runFunc func(ctx context.Context, c Config) (stats, error)

// Checker implements registry.Checker for ICMP/UDP ping checks.
type Checker struct {
	// run overrides the ping transport in tests; nil uses pro-bing.
	run runFunc
}

func init() {
	registry.Register(&Checker{})
}

// Type implements registry.Checker.
func (*Checker) Type() string { return TypeName }

// DefaultTimeout implements registry.TimeoutDefaulter.
func (*Checker) DefaultTimeout() time.Duration { return defaultTimeout }

// Validate implements registry.Checker.
func (*Checker) Validate(cfg json.RawMessage) error {
	c, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	return c.validate()
}

// Check implements registry.Checker: it runs the ping and maps the measured
// packet loss against the threshold. A failed run or excessive loss is a DOWN
// result; only an unusable config returns an error.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	run := ch.run
	if run == nil {
		run = runPing
	}
	st, err := run(ctx, c)
	if err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Message: err.Error()}, nil
	}

	status, message := assess(c, st)
	return domain.CheckResult{
		Status:  status,
		Latency: st.AvgRTT,
		Message: message,
		Extra:   extraJSON(st),
	}, nil
}

// assess maps ping statistics to a status and message given the config's loss
// threshold.
func assess(c Config, st stats) (domain.MonitorStatus, string) {
	switch {
	case st.PacketsRecv == 0:
		return domain.StatusDown, fmt.Sprintf("no reply: %d/%d packets lost (100%% loss)", st.PacketsSent, st.PacketsSent)
	case st.PacketLoss > c.lossThreshold():
		return domain.StatusDown, fmt.Sprintf("packet loss %.1f%% exceeds threshold %.1f%% (%d/%d received)",
			st.PacketLoss, c.lossThreshold(), st.PacketsRecv, st.PacketsSent)
	default:
		return domain.StatusUp, fmt.Sprintf("%d/%d replies, %.1f%% loss, avg %s",
			st.PacketsRecv, st.PacketsSent, st.PacketLoss, st.AvgRTT.Round(time.Microsecond))
	}
}

// runPing is the default pro-bing transport. It uses an unprivileged UDP ping
// so no raw-socket privileges are required.
func runPing(ctx context.Context, c Config) (stats, error) {
	pinger, err := probing.NewPinger(c.Host)
	if err != nil {
		return stats{}, fmt.Errorf("ping: resolve %q: %w", c.Host, err)
	}
	pinger.SetPrivileged(false) // unprivileged UDP ping
	pinger.Count = c.count()
	if deadline, ok := ctx.Deadline(); ok {
		pinger.Timeout = time.Until(deadline)
	}
	if err := pinger.RunWithContext(ctx); err != nil {
		return stats{}, fmt.Errorf("ping: %w", err)
	}
	s := pinger.Statistics()
	return stats{
		PacketsSent: s.PacketsSent,
		PacketsRecv: s.PacketsRecv,
		PacketLoss:  s.PacketLoss,
		AvgRTT:      s.AvgRtt,
		AvgRTTMs:    float64(s.AvgRtt) / float64(time.Millisecond),
	}, nil
}

// extraJSON marshals the ping statistics for the heartbeat Extra column.
func extraJSON(st stats) json.RawMessage {
	st.AvgRTTMs = float64(st.AvgRTT) / float64(time.Millisecond)
	data, err := json.Marshal(st)
	if err != nil {
		return nil
	}
	return data
}
