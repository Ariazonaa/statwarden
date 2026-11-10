package api

import (
	"context"
	"testing"
	"time"
)

// runIncidentStoreContract is the incident half of the Store contract, run
// against MemStore here and against PostgreSQL in
// pgstore_incidents_integration_test.go. It is kept beside runStoreContract
// rather than inside it so the incident queries can grow their own cases
// without every one of them paying for the whole table.
func runIncidentStoreContract(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	ctx := context.Background()

	newPage := func(t *testing.T, s Store, slug string) StatusPage {
		t.Helper()
		// Theme spelled out: status_pages.theme is NOT NULL with a CHECK.
		p, err := s.CreateStatusPage(ctx, StatusPage{Slug: slug, Title: slug, Theme: "auto"})
		if err != nil {
			t.Fatalf("CreateStatusPage(%s): %v", slug, err)
		}
		return p
	}

	t.Run("an incident created resolved keeps its resolution time", func(t *testing.T) {
		s := newStore(t)
		page := newPage(t, s, "created-resolved")
		resolved := time.Date(2026, 3, 14, 9, 10, 0, 0, time.UTC)
		in, err := s.CreateIncident(ctx, Incident{
			StatusPageID: page.ID, Title: "after the fact", Severity: "minor",
			Status: IncidentResolved, ResolvedAt: &resolved,
		})
		if err != nil {
			t.Fatalf("CreateIncident: %v", err)
		}
		got, err := s.GetIncident(ctx, in.ID)
		if err != nil {
			t.Fatalf("GetIncident: %v", err)
		}
		if got.ResolvedAt == nil || !got.ResolvedAt.Equal(resolved) {
			t.Errorf("resolved_at = %v, want %v", got.ResolvedAt, resolved)
		}
	})
}

func TestMemStoreSatisfiesTheIncidentContract(t *testing.T) {
	runIncidentStoreContract(t, func(*testing.T) Store { return NewMemStore() })
}
