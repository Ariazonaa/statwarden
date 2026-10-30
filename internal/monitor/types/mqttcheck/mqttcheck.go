// Package mqttcheck implements the MQTT monitor type: it connects to an MQTT
// broker and reports UP on a successful connection. When a Topic is configured
// it also subscribes and asserts that a message arrives, optionally matching an
// expected payload; a PublishPayload turns the check into a self-contained
// publish/subscribe round-trip. Connection, authentication and message-wait
// failures map to a DOWN CheckResult rather than a Go error.
package mqttcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "mqtt"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// probeResult carries the outcome of a live probe.
type probeResult struct {
	// subscribed is true when the config asked to wait for a topic message.
	subscribed bool
	// gotMessage is true when a message was received on the topic.
	gotMessage bool
	// payload is the received message payload, when gotMessage is true.
	payload string
}

// Checker implements registry.Checker for the MQTT family.
type Checker struct {
	// probe performs the live connect and optional subscribe/publish. nil uses
	// the real paho-backed implementation; tests inject a fake to exercise
	// result mapping without a broker.
	probe func(ctx context.Context, c Config) (probeResult, error)
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

// Check implements registry.Checker. Connection/auth/message failures are DOWN
// results; only unusable configuration returns an error.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	probe := ch.probe
	if probe == nil {
		probe = liveProbe
	}

	start := time.Now()
	res, err := probe(ctx, c)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}

	if msg := c.assess(res); msg != "" {
		return domain.CheckResult{Status: domain.StatusDown, Latency: latency, Message: msg}, nil
	}
	return domain.CheckResult{Status: domain.StatusUp, Latency: latency, Message: c.okMessage()}, nil
}

// assess applies the topic/payload assertions and returns a failure message, or
// "" when the check passes.
func (c Config) assess(res probeResult) string {
	if !res.subscribed {
		return ""
	}
	if !res.gotMessage {
		return fmt.Sprintf("no message received on topic %q within timeout", c.Topic)
	}
	if c.ExpectedPayload != "" && res.payload != c.ExpectedPayload {
		// The broker's payload is not echoed — see registry.Checker.Check.
		return fmt.Sprintf("message payload does not match the expected %q", c.ExpectedPayload)
	}
	return ""
}

// okMessage describes a successful check.
func (c Config) okMessage() string {
	if c.Topic != "" {
		return fmt.Sprintf("connected, message received on topic %q", c.Topic)
	}
	return "broker connection established"
}

// clientOptions builds the paho client options from the config. It performs no
// I/O and is unit-testable.
func (c Config) clientOptions() (*mqtt.ClientOptions, error) {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(c.Broker)
	if c.ClientID != "" {
		opts.SetClientID(c.ClientID)
	}
	if c.Username != "" {
		opts.SetUsername(c.Username)
		opts.SetPassword(c.Password)
	}
	opts.SetConnectTimeout(time.Duration(c.connectTimeout()) * time.Second)
	opts.SetAutoReconnect(false)
	if c.usesTLS() {
		tlsCfg, err := c.tlsConfig()
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsCfg)
	}
	return opts, nil
}

// liveProbe performs the real broker connection and optional round-trip.
func liveProbe(ctx context.Context, c Config) (probeResult, error) {
	timeout := time.Duration(c.connectTimeout()) * time.Second

	opts, err := c.clientOptions()
	if err != nil {
		return probeResult{}, err
	}
	client := mqtt.NewClient(opts)

	tok := client.Connect()
	if !tok.WaitTimeout(timeout) {
		return probeResult{}, fmt.Errorf("connect timed out after %s", timeout)
	}
	if err := tok.Error(); err != nil {
		return probeResult{}, err
	}
	defer client.Disconnect(250)

	if c.Topic == "" {
		return probeResult{}, nil
	}

	msgCh := make(chan mqtt.Message, 1)
	handler := func(_ mqtt.Client, m mqtt.Message) {
		select {
		case msgCh <- m:
		default:
		}
	}
	sub := client.Subscribe(c.Topic, 0, handler)
	if !sub.WaitTimeout(timeout) {
		return probeResult{}, fmt.Errorf("subscribe to %q timed out", c.Topic)
	}
	if err := sub.Error(); err != nil {
		return probeResult{}, err
	}

	if c.PublishPayload != "" {
		pub := client.Publish(c.Topic, 0, false, c.PublishPayload)
		if !pub.WaitTimeout(timeout) {
			return probeResult{}, fmt.Errorf("publish to %q timed out", c.Topic)
		}
		if err := pub.Error(); err != nil {
			return probeResult{}, err
		}
	}

	select {
	case m := <-msgCh:
		return probeResult{subscribed: true, gotMessage: true, payload: string(m.Payload())}, nil
	case <-ctx.Done():
		return probeResult{subscribed: true}, nil
	case <-time.After(timeout):
		return probeResult{subscribed: true}, nil
	}
}
