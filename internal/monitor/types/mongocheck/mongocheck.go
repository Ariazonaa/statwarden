// Package mongocheck implements the MongoDB monitor type: it connects with the
// official mongo-driver and issues a ping. Connection and authentication
// failures map to a DOWN CheckResult rather than a Go error.
package mongocheck

import (
	"context"
	"encoding/json"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "mongodb"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// Checker implements registry.Checker for the MongoDB family.
type Checker struct {
	// probe performs the live connection and ping. nil uses the real
	// driver-backed implementation; tests inject a fake.
	probe func(ctx context.Context, c Config) error
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

// Check implements registry.Checker. Connection/auth failures are DOWN
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
		probe = liveMongo
	}

	start := time.Now()
	err = probe(ctx, c)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}
	return domain.CheckResult{
		Status:  domain.StatusUp,
		Latency: latency,
		Message: "connection established, ping ok",
	}, nil
}

// liveMongo performs the real connection and ping.
func liveMongo(ctx context.Context, c Config) error {
	connCtx, cancel := context.WithTimeout(ctx, c.connectTimeout())
	defer cancel()

	client, err := mongo.Connect(connCtx, options.Client().ApplyURI(c.uri()))
	if err != nil {
		return err
	}
	// Disconnect on a fresh context so a cancelled probe still tears down.
	defer func() { _ = client.Disconnect(context.Background()) }()

	return client.Ping(connCtx, readpref.Primary())
}
