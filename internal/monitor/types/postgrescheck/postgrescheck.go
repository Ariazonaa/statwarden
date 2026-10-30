// Package postgrescheck implements the PostgreSQL monitor type: it opens a
// connection with the pgx driver already used by Statwarden's own database layer
// and, optionally, runs an assertion query whose first column is compared
// against an expected value. Connection, authentication and query failures
// map to a DOWN CheckResult rather than a Go error, so a broken target is a
// normal monitoring outcome, not a checker malfunction.
package postgrescheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "postgres"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// probeResult carries the outcome of a live probe: the stringified first
// column of the assertion query, when one ran.
type probeResult struct {
	value    string
	hasValue bool
}

// Checker implements registry.Checker for the PostgreSQL family.
type Checker struct {
	// probe performs the live connection and optional query. nil uses the
	// real pgx-backed implementation; tests inject a fake to exercise result
	// mapping without a live database.
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

// Check implements registry.Checker. Connection/auth/query failures are DOWN
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
		probe = livePostgres
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

// assess applies the optional query-result assertion and returns a failure
// message, or "" when the check passes.
func (c Config) assess(res probeResult) string {
	if c.ExpectedResult == "" {
		return ""
	}
	if !res.hasValue {
		return "assertion query returned no rows"
	}
	if res.value != c.ExpectedResult {
		// The observed value is not echoed — see registry.Checker.Check.
		return fmt.Sprintf("query result does not match the expected %q", c.ExpectedResult)
	}
	return ""
}

// okMessage describes a successful check.
func (c Config) okMessage() string {
	if c.Query != "" {
		return "connection established, query executed"
	}
	return "connection established"
}

// livePostgres performs the real connection and optional query with pgx.
func livePostgres(ctx context.Context, c Config) (probeResult, error) {
	connCtx, cancel := context.WithTimeout(ctx, time.Duration(c.connectTimeout())*time.Second)
	defer cancel()

	conn, err := pgx.Connect(connCtx, c.dsn())
	if err != nil {
		return probeResult{}, err
	}
	// Close on a fresh context so a cancelled probe still tears down cleanly.
	defer func() { _ = conn.Close(context.Background()) }()

	if c.Query == "" {
		if err := conn.Ping(connCtx); err != nil {
			return probeResult{}, err
		}
		return probeResult{}, nil
	}

	// The assertion query runs in a READ ONLY transaction.
	//
	// It is operator-supplied SQL executed against operator-supplied
	// credentials, and the credentials are whatever the monitor was given —
	// commonly a full application account rather than a reporting role. Nothing
	// constrained the statement, so "assertion query" also accepted UPDATE,
	// DELETE, a writing function, or SELECT ... RETURNING, and anyone who could
	// edit a monitor could write to the database it monitors.
	//
	// A READ ONLY transaction is the right instrument rather than inspecting
	// the SQL: PostgreSQL enforces it on the server, across CTEs, functions and
	// anything else a statement parser in this package would have to chase and
	// would eventually be tricked by.
	tx, err := conn.BeginTx(connCtx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return probeResult{}, err
	}
	// Always rolled back: a read-only transaction has nothing to commit, and
	// rolling back costs one round trip that the deferred close would make
	// anyway.
	defer func() { _ = tx.Rollback(context.Background()) }()

	var value any
	if err := tx.QueryRow(connCtx, c.Query).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return probeResult{}, nil
		}
		return probeResult{}, err
	}
	return probeResult{value: stringify(value), hasValue: true}, nil
}

// stringify renders a scanned column value as a string for comparison.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}
