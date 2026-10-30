// Package mysqlcheck implements the MySQL/MariaDB monitor type: it opens a
// database/sql connection with the go-sql-driver/mysql driver and, optionally,
// runs an assertion query whose first column is compared against an expected
// value. Connection, authentication and query failures map to a DOWN
// CheckResult rather than a Go error.
package mysqlcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" database/sql driver

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "mysql"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// probeResult carries the outcome of a live probe: the stringified first
// column of the assertion query, when one ran.
type probeResult struct {
	value    string
	hasValue bool
}

// Checker implements registry.Checker for the MySQL/MariaDB family.
type Checker struct {
	// probe performs the live connection and optional query. nil uses the
	// real driver-backed implementation; tests inject a fake.
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
		probe = liveMySQL
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

// assess applies the optional query-result assertion.
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

// liveMySQL performs the real connection and optional query.
func liveMySQL(ctx context.Context, c Config) (probeResult, error) {
	connCtx, cancel := context.WithTimeout(ctx, c.connectTimeout())
	defer cancel()

	db, err := sql.Open("mysql", c.dsn())
	if err != nil {
		return probeResult{}, err
	}
	defer func() { _ = db.Close() }()

	if c.Query == "" {
		if err := db.PingContext(connCtx); err != nil {
			return probeResult{}, err
		}
		return probeResult{}, nil
	}

	// READ ONLY transaction — see the equivalent comment in postgrescheck. The
	// assertion query is operator-supplied SQL running with the monitor's
	// stored credentials, and nothing stopped it writing.
	//
	// MySQL has supported START TRANSACTION READ ONLY since 5.6. On anything
	// older BeginTx fails and the check reports DOWN with the server's error,
	// which is the correct direction to fail in: a monitor that cannot be run
	// safely should say so rather than quietly run unrestricted.
	tx, err := db.BeginTx(connCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return probeResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var value any
	if err := tx.QueryRowContext(connCtx, c.Query).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeResult{}, nil
		}
		return probeResult{}, err
	}
	return probeResult{value: stringify(value), hasValue: true}, nil
}

// stringify renders a scanned column value as a string for comparison. The
// database/sql driver returns integers/floats natively and text/blob columns
// as []byte.
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
