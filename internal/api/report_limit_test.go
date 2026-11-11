package api

import (
	"net/http"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/report"
)

// fill occupies every slot of a report gate and returns the function that
// frees them again.
func fill(g reportGate) func() {
	for i := 0; i < cap(g); i++ {
		g <- struct{}{}
	}
	return func() {
		for i := 0; i < cap(g); i++ {
			<-g
		}
	}
}

// Report endpoints read heartbeats per monitor, and nothing bounded how many
// ran at once: any authenticated user could start as many fleet-wide SLA
// reports as they had connections. Past the limit a request is refused with
// 503 and Retry-After rather than queued, so the work in flight stays bounded.
func TestReportEndpointsAreConcurrencyLimited(t *testing.T) {
	rep := &fakeReporter{report: report.FullReport{Uptime: report.UptimeResult{Up: 1, Total: 1, Uptime: 1}}}
	h := newExtHarness(t, rep, nil)
	admin := h.setupAdmin()
	monitor := "/api/v1/monitors/" + itoa64(h.createMonitorFake(admin)) + "/report"

	release := fill(fleetReports)
	for _, path := range []string{"/api/v1/report/sla.csv", "/api/v1/report/sla.pdf", "/api/v1/report/flakiest"} {
		rec := h.requestWith(http.MethodGet, path, nil, admin)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s with every slot taken = %d (Retry-After %q), want 503 with Retry-After",
				path, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	// A per-monitor report has its own, separate allowance.
	if rec := h.requestWith(http.MethodGet, monitor, nil, admin); rec.Code != http.StatusOK {
		t.Errorf("a per-monitor report while the fleet slots are taken = %d, want 200", rec.Code)
	}
	release()

	release = fill(monitorReports)
	if rec := h.requestWith(http.MethodGet, monitor, nil, admin); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a per-monitor report with every slot taken = %d, want 503", rec.Code)
	}
	release()

	// Slots are returned after each request.
	for i := 0; i < cap(fleetReports)+1; i++ {
		if rec := h.requestWith(http.MethodGet, "/api/v1/report/sla.csv", nil, admin); rec.Code != http.StatusOK {
			t.Fatalf("sequential report %d = %d, want 200: a slot leaked", i, rec.Code)
		}
	}
}
