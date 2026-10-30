package sslcheck

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/db/sqlcgen"
)

// PgCertSource implements Source against the cert_info table.
type PgCertSource struct {
	q *sqlcgen.Queries
}

// NewPgCertSource wraps a pgx pool in a Source.
func NewPgCertSource(pool *pgxpool.Pool) *PgCertSource {
	return &PgCertSource{q: sqlcgen.New(pool)}
}

// ListCertInfo implements Source: certificates of active monitors with a
// known expiry date.
func (s *PgCertSource) ListCertInfo(ctx context.Context) ([]CertRecord, error) {
	rows, err := s.q.ListCertInfo(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]CertRecord, 0, len(rows))
	for _, r := range rows {
		rec := CertRecord{
			MonitorID:   r.MonitorID,
			MonitorName: r.MonitorName,
			Issuer:      r.Issuer.String,
		}
		if r.ValidTo.Valid {
			rec.ValidTo = r.ValidTo.Time
		}
		records = append(records, rec)
	}
	return records, nil
}
