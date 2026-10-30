package api

import (
	"context"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/push"
	"github.com/Ariazonaa/statwarden/internal/report"
	"github.com/Ariazonaa/statwarden/internal/siem"
)

// Reporter is the slice of the reporting service the API drives: uptime,
// per-monitor reports, the flakiest ranking and the certificate overview.
// *report.Service satisfies it; handler tests use a fake.
type Reporter interface {
	Uptime(ctx context.Context, monitorID int64, r report.Range) (report.UptimeResult, error)
	// UptimeSeries answers a whole run of contiguous buckets in a bounded number
	// of queries. The public status page renders up to 168 bars per monitor, so
	// calling Uptime once per bar is the difference between a handful of queries
	// per page view and several thousand.
	UptimeSeries(ctx context.Context, monitorID int64, buckets []report.Range) ([]report.UptimeResult, error)
	Report(ctx context.Context, monitorID int64, r report.Range) (report.FullReport, error)
	Flakiest(ctx context.Context, r report.Range, limit int) ([]report.Flakiness, error)
	CertOverview(ctx context.Context) ([]report.CertRow, error)
}

// PushRecorder records an incoming push heartbeat. *push.Recorder satisfies it;
// handler tests use a fake. push.ErrNotFound signals an unknown token.
type PushRecorder interface {
	Record(ctx context.Context, token string, beat push.Beat) (domain.Monitor, error)
}

// pushTokenStore is the extra persistence the push-token (re)generation
// endpoint needs on top of the base Store. Both MemStore and PgStore implement
// it.
type pushTokenStore interface {
	SetPushToken(ctx context.Context, monitorID int64, token string) error
}

// AuditSink receives every persisted audit entry for forwarding to an external
// collector. *siem.Forwarder satisfies it; handler tests use a fake.
//
// The contract is the reason this is an interface and not a direct dependency:
// HandleAudit MUST NOT block, MUST NOT fail, and MUST NOT be able to affect the
// request that produced the event. A nil sink is the normal state of an instance
// with no collector configured.
type AuditSink interface {
	HandleAudit(ctx context.Context, e siem.Event)
}
