package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

func TestRenderDefaults(t *testing.T) {
	r := NewRenderer(nil)
	at := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	lat := 142 * time.Millisecond

	tests := []struct {
		name       string
		ev         domain.NotificationEvent
		wantTitle  string
		wantInBody []string
	}{
		{
			name: "down",
			ev: domain.NotificationEvent{
				Kind: domain.EventKindDown, MonitorName: "API", MonitorURL: "https://api.example.com",
				Status: domain.StatusDown, Message: "500", Latency: &lat, At: at,
			},
			wantTitle:  "[Statwarden] API is DOWN",
			wantInBody: []string{"API", "https://api.example.com", "DOWN", "500", "2026-07-20 08:30:00 UTC"},
		},
		{
			name: "up with downtime",
			ev: func() domain.NotificationEvent {
				since := at.Add(-5 * time.Minute)
				return domain.NotificationEvent{
					Kind: domain.EventKindUp, MonitorName: "API", Status: domain.StatusUp,
					At: at, DownSince: &since,
				}
			}(),
			wantTitle:  "[Statwarden] API is UP",
			wantInBody: []string{"UP again", "5m0s"},
		},
		{
			name: "cert expiry",
			ev: domain.NotificationEvent{
				Kind: domain.EventKindCertExpiry, MonitorName: "web",
				CertDaysRemaining: 7, CertExpiresAt: at, At: at,
			},
			wantTitle:  "[Statwarden] Certificate for web expires in 7 day(s)",
			wantInBody: []string{"expires in 7 day(s)"},
		},
		{
			name:       "test",
			ev:         domain.NotificationEvent{Kind: domain.EventKindTest, At: at},
			wantTitle:  "[Statwarden] Test notification",
			wantInBody: []string{"test notification"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, body := r.Render(tt.ev, nil)
			if title != tt.wantTitle {
				t.Fatalf("title = %q, want %q", title, tt.wantTitle)
			}
			for _, want := range tt.wantInBody {
				if !strings.Contains(body, want) {
					t.Fatalf("body %q missing %q", body, want)
				}
			}
		})
	}
}

func TestRenderReminderAndEscalation(t *testing.T) {
	r := NewRenderer(nil)
	at := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	since := at.Add(-10 * time.Minute)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "db", Status: domain.StatusDown,
		At: at, DownSince: &since, Reminder: true, Escalation: true,
	}
	_, body := r.Render(ev, nil)
	for _, want := range []string{"still down since", "10m0s", "escalation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q missing %q", body, want)
		}
	}
}

func TestRenderOverride(t *testing.T) {
	r := NewRenderer(nil)
	overrides := map[string]MessageTemplate{
		"down": {Title: "DOWN: {{.MonitorName}}", Body: "custom {{.Status}}"},
	}
	ev := domain.NotificationEvent{Kind: domain.EventKindDown, MonitorName: "API", Status: domain.StatusDown}
	title, body := r.Render(ev, overrides)
	if title != "DOWN: API" {
		t.Fatalf("title = %q", title)
	}
	if body != "custom down" {
		t.Fatalf("body = %q", body)
	}
}

func TestRenderBrokenOverrideFallsBack(t *testing.T) {
	r := NewRenderer(nil)
	overrides := map[string]MessageTemplate{
		"down": {Title: "{{.Nonexistent.Field}}", Body: "{{if broken"},
	}
	ev := domain.NotificationEvent{Kind: domain.EventKindDown, MonitorName: "API", Status: domain.StatusDown, At: time.Now()}
	title, body := r.Render(ev, overrides)
	// Broken override must fall back to the default template, not lose the alert.
	if !strings.Contains(title, "API is DOWN") {
		t.Fatalf("title did not fall back to default: %q", title)
	}
	if !strings.Contains(body, "API") {
		t.Fatalf("body did not fall back to default: %q", body)
	}
}

func TestRenderDigest(t *testing.T) {
	r := NewRenderer(nil)
	at := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDigest, At: at,
		Items: []domain.NotificationEvent{
			{Kind: domain.EventKindDown, MonitorName: "API", Status: domain.StatusDown, At: at},
			{Kind: domain.EventKindUp, MonitorName: "web", Status: domain.StatusUp, At: at},
		},
	}
	title, body := r.Render(ev, nil)
	if !strings.Contains(title, "2 event(s)") {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(body, "API") || !strings.Contains(body, "web") {
		t.Fatalf("digest body missing monitors: %q", body)
	}
}
