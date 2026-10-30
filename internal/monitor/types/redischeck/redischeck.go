// Package redischeck implements the Redis monitor type: it connects with
// go-redis and issues a PING. Connection and authentication failures map to a
// DOWN CheckResult rather than a Go error.
package redischeck

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "redis"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// Checker implements registry.Checker for the Redis family.
type Checker struct {
	// probe performs the live connection and PING. nil uses the real
	// go-redis-backed implementation; tests inject a fake.
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
		probe = liveRedis
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
		Message: "connection established, PING ok",
	}, nil
}

// liveRedis performs the real connection and PING.
func liveRedis(ctx context.Context, c Config) error {
	connCtx, cancel := context.WithTimeout(ctx, c.connectTimeout())
	defer cancel()

	opts := &redis.Options{
		Addr:        c.addr(),
		Username:    c.Username,
		Password:    c.Password,
		DB:          c.Database,
		DialTimeout: c.connectTimeout(),
	}
	if c.TLS {
		opts.TLSConfig = &tls.Config{
			ServerName:         c.Host,
			InsecureSkipVerify: c.TLSSkipVerify, //nolint:gosec // opt-in for self-signed internal Redis
		}
	}

	client := redis.NewClient(opts)
	defer func() { _ = client.Close() }()

	return client.Ping(connCtx).Err()
}
