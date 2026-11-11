package api

import (
	"net/http"
	"testing"
)

// Every numeric field a monitor write carries is bounded in validateMonitor.
//
// Only interval and recovery_checks used to be. The rest went to the store
// unchecked, where PostgreSQL's CHECK constraints and int4 columns rejected them
// as a 500 — while MemStore, which has neither, stored them — and a resend
// interval large enough wrapped the minute Duration the engine computes.
func TestMonitorNumericFieldsAreBounded(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	intp := func(n int) *int { return &n }

	cases := []struct {
		name string
		mut  func(*monitorRequest)
	}{
		{"interval past the ceiling", func(r *monitorRequest) { r.Interval = maxMonitorSeconds + 1 }},
		{"zero timeout", func(r *monitorRequest) { r.Timeout = intp(0) }},
		{"negative timeout", func(r *monitorRequest) { r.Timeout = intp(-5) }},
		{"timeout past the ceiling", func(r *monitorRequest) { r.Timeout = intp(maxMonitorSeconds + 1) }},
		{"negative max_retries", func(r *monitorRequest) { r.MaxRetries = -1 }},
		{"max_retries past the ceiling", func(r *monitorRequest) { r.MaxRetries = maxMonitorRetries + 1 }},
		{"negative retry interval", func(r *monitorRequest) { r.RetryInterval = -1 }},
		{"retry interval past the ceiling", func(r *monitorRequest) { r.RetryInterval = maxMonitorSeconds + 1 }},
		{"negative resend interval", func(r *monitorRequest) { r.ResendInterval = -1 }},
		{"resend interval past the ceiling", func(r *monitorRequest) { r.ResendInterval = maxResendMinutes + 1 }},
		{"unknown tag", func(r *monitorRequest) { r.TagIDs = []int64{9999} }},
		{"unknown notification", func(r *monitorRequest) { r.NotificationIDs = []int64{9999} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := monitorRequest{Name: "m", Type: "fake", Interval: 60}
			c.mut(&req)
			rec := h.requestWith(http.MethodPost, "/api/v1/monitors", req, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}

	// The defaults the web form sends stay valid: no timeout, a zero retry
	// interval meaning "same as interval", no resend, and the ceilings themselves.
	ok := monitorRequest{
		Name: "ok", Type: "fake", Interval: maxMonitorSeconds,
		MaxRetries: maxMonitorRetries, ResendInterval: maxResendMinutes,
	}
	if rec := h.requestWith(http.MethodPost, "/api/v1/monitors", ok, admin); rec.Code != http.StatusCreated {
		t.Fatalf("create at the ceilings = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	// Update goes through the same checks.
	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{Name: "u", Type: "fake", Interval: 60}, admin)
	var created monitorView
	mustJSON(t, rec, &created)
	rec = h.requestWith(http.MethodPut, monitorPath(created.ID, ""), monitorRequest{
		Name: "u", Type: "fake", Interval: 60, TagIDs: []int64{9999},
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with an unknown tag = %d, want 400", rec.Code)
	}
}
