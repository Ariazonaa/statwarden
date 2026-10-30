package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// MonitorCreateRequest is the create payload.
//
// CONTRACT: every json tag here must exist on api.monitorRequest
// (internal/api/monitors.go). The server decodes with DisallowUnknownFields, so
// a tag this struct sends and the server does not know is not silently
// ignored — it is an unconditional HTTP 400 on every create. A tag renamed on
// the server side breaks this client with nothing in either build failing.
//
// The DTO is redeclared rather than imported because internal/api pulls in chi,
// pgx and the whole router; a CLI that only needs net/http should not link a
// PostgreSQL driver.
//
// Pointers are load-bearing: Active and Timeout are pointers on the server too,
// so a value type here would force active=false onto every created monitor
// instead of letting the server apply its default. Everything else is
// omitempty so the server's own defaulting (retry_interval_seconds falls back
// to interval_seconds, config falls back to `{}`) stays in one place.
type MonitorCreateRequest struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Config          json.RawMessage `json:"config,omitempty"`
	Interval        int             `json:"interval_seconds,omitempty"`
	Timeout         *int            `json:"timeout_seconds,omitempty"`
	MaxRetries      int             `json:"max_retries,omitempty"`
	RetryInterval   int             `json:"retry_interval_seconds,omitempty"`
	ResendInterval  int             `json:"resend_interval_minutes,omitempty"`
	UpsideDown      bool            `json:"upside_down,omitempty"`
	Active          *bool           `json:"active,omitempty"`
	ParentID        *int64          `json:"parent_id,omitempty"`
	GroupID         *int64          `json:"group_id,omitempty"`
	Description     string          `json:"description,omitempty"`
	LinkURL         string          `json:"link_url,omitempty"`
	TagIDs          []int64         `json:"tag_ids,omitempty"`
	NotificationIDs []int64         `json:"notification_ids,omitempty"`
}

// MonitorView is the read shape returned by the API, mirroring api.monitorView.
// It is a superset of the write shape plus derived state (status, uptime,
// latency). Unknown fields are ignored on decode, so a server that grows a
// field does not break this client.
type MonitorView struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Config          json.RawMessage `json:"config"`
	Interval        int             `json:"interval_seconds"`
	Timeout         *int            `json:"timeout_seconds"`
	MaxRetries      int             `json:"max_retries"`
	RetryInterval   int             `json:"retry_interval_seconds"`
	ResendInterval  int             `json:"resend_interval_minutes"`
	UpsideDown      bool            `json:"upside_down"`
	Active          bool            `json:"active"`
	Status          string          `json:"status"`
	ParentID        *int64          `json:"parent_id"`
	GroupID         *int64          `json:"group_id"`
	Description     string          `json:"description"`
	LinkURL         string          `json:"link_url"`
	TagIDs          []int64         `json:"tag_ids"`
	NotificationIDs []int64         `json:"notification_ids"`
	URL             string          `json:"url"`
	CreatedAt       time.Time       `json:"created_at"`

	LatencyMs     *int       `json:"latency_ms"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	Uptime24h     *float64   `json:"uptime_24h"`
	Uptime30d     *float64   `json:"uptime_30d"`
	Uptime1y      *float64   `json:"uptime_1y"`
}

// MonitorList is the paginated list envelope shared by every list endpoint,
// mirroring api.listEnvelope.
type MonitorList struct {
	Items  []MonitorView `json:"items"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// MonitorFilter narrows a list request. Zero values are omitted.
type MonitorFilter struct {
	Status string
	Type   string
	Search string
	Limit  int
	Offset int
}

func (f MonitorFilter) query() url.Values {
	q := url.Values{}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.Type != "" {
		q.Set("type", f.Type)
	}
	if f.Search != "" {
		q.Set("search", f.Search)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Offset > 0 {
		q.Set("offset", strconv.Itoa(f.Offset))
	}
	return q
}

// ListMonitors returns one page of monitors.
func (c *client) ListMonitors(ctx context.Context, f MonitorFilter) (MonitorList, error) {
	var out MonitorList
	err := c.do(ctx, "GET", "/monitors", f.query(), nil, &out)
	return out, err
}

// GetMonitor returns a single monitor.
func (c *client) GetMonitor(ctx context.Context, id int64) (MonitorView, error) {
	var out MonitorView
	err := c.do(ctx, "GET", "/monitors/"+strconv.FormatInt(id, 10), nil, nil, &out)
	return out, err
}

// CreateMonitor creates a monitor and returns the server's view of it.
func (c *client) CreateMonitor(ctx context.Context, req MonitorCreateRequest) (MonitorView, error) {
	var out MonitorView
	err := c.do(ctx, "POST", "/monitors", nil, req, &out)
	return out, err
}

// SetMonitorActive pauses or resumes a monitor.
func (c *client) SetMonitorActive(ctx context.Context, id int64, active bool) error {
	action := "pause"
	if active {
		action = "resume"
	}
	return c.do(ctx, "POST", fmt.Sprintf("/monitors/%d/%s", id, action), nil, nil, nil)
}

// DeleteMonitor removes a monitor and its history.
func (c *client) DeleteMonitor(ctx context.Context, id int64) error {
	return c.do(ctx, "DELETE", "/monitors/"+strconv.FormatInt(id, 10), nil, nil, nil)
}
