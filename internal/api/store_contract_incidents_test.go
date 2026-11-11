package api

import (
	"context"
	"reflect"
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

	// Two pages, incidents in every state the public selection distinguishes,
	// each carrying its own monitors and timeline so a batched detail load that
	// attaches rows to the wrong incident is visible.
	type fixture struct {
		s      Store
		page   StatusPage
		byName map[string]int64
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		s := newStore(t)
		page := newPage(t, s, "public-incidents")
		other := newPage(t, s, "someone-else")
		monA, err := s.CreateMonitor(ctx, MonitorInput{
			Name: "a", Type: "http", Interval: 60, RetryInterval: 60, Active: true, Config: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("CreateMonitor: %v", err)
		}
		monB, err := s.CreateMonitor(ctx, MonitorInput{
			Name: "b", Type: "http", Interval: 60, RetryInterval: 60, Active: true, Config: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("CreateMonitor: %v", err)
		}
		f := fixture{s: s, page: page, byName: map[string]int64{}}
		published := time.Date(2026, 3, 14, 9, 10, 0, 0, time.UTC)
		// Oldest first: creation order is the started_at order on both sides.
		for _, c := range []struct {
			name     string
			pageID   int64
			status   string
			pinned   bool
			pm       bool
			monitors []int64
		}{
			{"pinned-resolved", page.ID, IncidentResolved, true, false, []int64{monA.ID}},
			{"pm1", page.ID, IncidentResolved, false, true, nil},
			{"pm2", page.ID, IncidentResolved, false, true, []int64{monB.ID}},
			{"pm3", page.ID, IncidentResolved, false, true, nil},
			{"n1", page.ID, IncidentResolved, false, false, nil},
			{"n2", page.ID, IncidentResolved, false, false, []int64{monA.ID, monB.ID}},
			{"n3", page.ID, IncidentResolved, false, false, nil},
			{"n4", page.ID, IncidentResolved, false, false, []int64{monB.ID}},
			{"n5", page.ID, IncidentResolved, false, false, nil},
			{"active-pinned", page.ID, IncidentInvestigating, true, false, []int64{monA.ID}},
			{"active", page.ID, IncidentMonitoring, false, false, nil},
			{"elsewhere", other.ID, IncidentInvestigating, false, false, nil},
		} {
			in, err := s.CreateIncident(ctx, Incident{
				StatusPageID: c.pageID, Title: c.name, Body: "opened " + c.name, Severity: "minor",
				Status: c.status, Pinned: c.pinned, MonitorIDs: c.monitors,
			})
			if err != nil {
				t.Fatalf("CreateIncident(%s): %v", c.name, err)
			}
			if _, err := s.AddIncidentUpdate(ctx, IncidentUpdate{
				IncidentID: in.ID, Status: c.status, Body: "update on " + c.name,
			}); err != nil {
				t.Fatalf("AddIncidentUpdate(%s): %v", c.name, err)
			}
			if c.pm {
				if _, err := s.SetIncidentPostmortem(ctx, in.ID, "write-up "+c.name, &published); err != nil {
					t.Fatalf("SetIncidentPostmortem(%s): %v", c.name, err)
				}
			}
			f.byName[c.name] = in.ID
			// Distinct started_at on both sides: PostgreSQL stamps now() per
			// transaction, MemStore reads the wall clock.
			time.Sleep(2 * time.Millisecond)
		}
		return f
	}

	// assertDetails checks each listed incident against GetIncident, which
	// loads one incident at a time and is therefore the reference.
	assertDetails := func(t *testing.T, s Store, list []Incident) {
		t.Helper()
		for _, in := range list {
			want, err := s.GetIncident(ctx, in.ID)
			if err != nil {
				t.Fatalf("GetIncident(%d): %v", in.ID, err)
			}
			if !reflect.DeepEqual(in.MonitorIDs, want.MonitorIDs) {
				t.Errorf("%s: monitor_ids = %v, want %v", in.Title, in.MonitorIDs, want.MonitorIDs)
			}
			if len(in.Updates) != len(want.Updates) {
				t.Errorf("%s: %d updates, want %d", in.Title, len(in.Updates), len(want.Updates))
				continue
			}
			for i := range in.Updates {
				if in.Updates[i].ID != want.Updates[i].ID || in.Updates[i].IncidentID != in.ID {
					t.Errorf("%s: update %d = %+v, want %+v", in.Title, i, in.Updates[i], want.Updates[i])
				}
			}
		}
	}

	t.Run("listing attaches each incident's own monitors and timeline", func(t *testing.T) {
		f := newFixture(t)
		list, err := f.s.ListIncidents(ctx, f.page.ID)
		if err != nil {
			t.Fatalf("ListIncidents: %v", err)
		}
		if len(list) != 11 {
			t.Fatalf("ListIncidents returned %d incidents, want the page's 11", len(list))
		}
		assertDetails(t, f.s, list)
	})

	t.Run("the public listing is bounded and keeps published post-mortems", func(t *testing.T) {
		f := newFixture(t)
		// Three recent resolved incidents, five resolved at most. Newest first,
		// pinned first, the resolved run is pinned-resolved, n5, n4, n3, n2, n1,
		// pm3, pm2, pm1: the first three are taken outright, then only those
		// with a published post-mortem until five.
		got, err := f.s.ListPublicIncidents(ctx, f.page.ID, 3, 5)
		if err != nil {
			t.Fatalf("ListPublicIncidents: %v", err)
		}
		want := map[int64]bool{}
		for _, name := range []string{"active-pinned", "active", "pinned-resolved", "n5", "n4", "pm3", "pm2"} {
			want[f.byName[name]] = true
		}
		gotIDs := map[int64]bool{}
		for _, in := range got {
			gotIDs[in.ID] = true
		}
		if !reflect.DeepEqual(gotIDs, want) {
			names := make([]string, 0, len(got))
			for _, in := range got {
				names = append(names, in.Title)
			}
			t.Fatalf("ListPublicIncidents = %v, want active-pinned, active, pinned-resolved, n5, n4, pm3, pm2", names)
		}

		// Same order as the full listing.
		all, err := f.s.ListIncidents(ctx, f.page.ID)
		if err != nil {
			t.Fatalf("ListIncidents: %v", err)
		}
		var wantOrder []int64
		for _, in := range all {
			if want[in.ID] {
				wantOrder = append(wantOrder, in.ID)
			}
		}
		gotOrder := make([]int64, 0, len(got))
		for _, in := range got {
			gotOrder = append(gotOrder, in.ID)
		}
		if !reflect.DeepEqual(gotOrder, wantOrder) {
			t.Errorf("order = %v, want %v (pinned first, newest first)", gotOrder, wantOrder)
		}
		assertDetails(t, f.s, got)
	})
}

func TestMemStoreSatisfiesTheIncidentContract(t *testing.T) {
	runIncidentStoreContract(t, func(*testing.T) Store { return NewMemStore() })
}
