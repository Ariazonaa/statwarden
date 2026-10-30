package notify

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// MessageTemplate is one per-event-kind override, stored in
// notifications.templates as {"down": {"title": "...", "body": "..."}, ...}.
// Empty fields fall back to the built-in default.
type MessageTemplate struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// TemplateData is the variable set available to message templates.
type TemplateData struct {
	Kind        string
	MonitorName string
	MonitorURL  string
	Status      string
	// Latency is a human-readable duration ("142ms"); empty when unknown.
	Latency string
	Message string
	// Timestamp is the event time formatted as "2006-01-02 15:04:05 UTC".
	Timestamp string

	Reminder   bool
	Escalation bool
	// DownSince/DownDuration are set on up events and down reminders.
	DownSince    string
	DownDuration string

	CertDaysRemaining int
	CertExpiresAt     string

	// Response-time anomaly context. Baseline is human-readable like Latency;
	// Deviations is how many standard deviations above it the check sat, and
	// DeviationsKnown is false when the baseline had no variance to measure
	// against (the score is then +Inf, which no template should print).
	AnomalyBaseline   string
	AnomalyDeviations float64
	DeviationsKnown   bool
	AnomalyRecovered  bool

	// Flapping context. FlapChanges is how many state changes the summary
	// covers, FlapWindow the detection window ("30m"), and Status carries
	// the state the monitor is in.
	FlapChanges int
	FlapWindow  string

	// Items carries the batched events of a digest.
	Items []TemplateData
}

// Default templates per event kind. Deliberately plain text so every provider
// (mail subject lines, chat messages, SMS) can carry them unmodified.
var defaultTemplates = map[domain.NotificationEventKind]MessageTemplate{
	domain.EventKindDown: {
		Title: "[Statwarden] {{.MonitorName}} is DOWN",
		Body: "Monitor {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} is DOWN" +
			"{{if .Reminder}} — still down since {{.DownSince}} ({{.DownDuration}}){{end}}" +
			"{{if .Escalation}} — escalation{{end}}" +
			"{{if .Message}}: {{.Message}}{{end}} [{{.Timestamp}}]",
	},
	domain.EventKindUp: {
		Title: "[Statwarden] {{.MonitorName}} is UP",
		Body: "Monitor {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} is UP again" +
			"{{if .DownDuration}} after {{.DownDuration}} of downtime{{end}}" +
			"{{if .Message}}: {{.Message}}{{end}} [{{.Timestamp}}]",
	},
	domain.EventKindCertExpiry: {
		Title: "[Statwarden] Certificate for {{.MonitorName}} expires in {{.CertDaysRemaining}} day(s)",
		Body: "The TLS certificate for {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} " +
			"{{if lt .CertDaysRemaining 0}}has EXPIRED{{else}}expires in {{.CertDaysRemaining}} day(s){{end}}" +
			" (valid until {{.CertExpiresAt}}){{if .Message}}: {{.Message}}{{end}}",
	},
	domain.EventKindMaintenanceStart: {
		Title: "[Statwarden] Maintenance started: {{.MonitorName}}",
		Body:  "A maintenance window for {{.MonitorName}} has started; alerts are suppressed until it ends. [{{.Timestamp}}]",
	},
	domain.EventKindMaintenanceEnd: {
		Title: "[Statwarden] Maintenance ended: {{.MonitorName}}",
		Body:  "The maintenance window for {{.MonitorName}} has ended; alerting has resumed. [{{.Timestamp}}]",
	},
	domain.EventKindTest: {
		Title: "[Statwarden] Test notification",
		Body:  "This is a test notification from Statwarden. If you can read this, the channel works. [{{.Timestamp}}]",
	},
	domain.EventKindDigest: {
		Title: "[Statwarden] Status digest: {{len .Items}} event(s)",
		Body: "Status summary [{{.Timestamp}}]:\n" +
			"{{range .Items}}- {{.Timestamp}} {{.MonitorName}}: {{.Status}}{{if .Message}} ({{.Message}}){{end}}\n{{end}}",
	},
	// The wording says "still UP" out loud: the whole point of an anomaly alert is
	// that it is not an outage, and an operator skim-reading a pager message must
	// not mistake it for one.
	domain.EventKindLatencyAnomaly: {
		Title: "[Statwarden] {{.MonitorName}} is slow (still up)",
		Body: "Monitor {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} is still UP but " +
			"responding unusually slowly{{if .Latency}}: {{.Latency}}{{end}}" +
			"{{if .AnomalyBaseline}} against a baseline of {{.AnomalyBaseline}}{{end}}" +
			"{{if .DeviationsKnown}} ({{printf \"%.1f\" .AnomalyDeviations}}σ above){{end}}" +
			" [{{.Timestamp}}]",
	},
	domain.EventKindLatencyNormal: {
		Title: "[Statwarden] {{.MonitorName}} response time back to normal",
		Body: "Monitor {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} is responding " +
			"normally again{{if .Latency}}: {{.Latency}}{{end}}" +
			"{{if .AnomalyBaseline}} against a baseline of {{.AnomalyBaseline}}{{end}}" +
			" [{{.Timestamp}}]",
	},
	// One message instead of one per bounce, and it says what happens next: the
	// individual alerts are held back, so the operator must not read silence as
	// "it stopped".
	domain.EventKindFlapping: {
		Title: "[Statwarden] {{.MonitorName}} is flapping",
		Body: "Monitor {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} changed state " +
			"{{.FlapChanges}} times within {{.FlapWindow}} and is currently {{.Status}}. " +
			"Individual up/down alerts are paused until it has been stable for {{.FlapWindow}}. [{{.Timestamp}}]",
	},
	domain.EventKindFlappingStable: {
		Title: "[Statwarden] {{.MonitorName}} has stopped flapping ({{.Status}})",
		Body: "Monitor {{.MonitorName}}{{if .MonitorURL}} ({{.MonitorURL}}){{end}} has been stable for " +
			"{{.FlapWindow}} and is {{.Status}}; {{.FlapChanges}} state change(s) were not alerted " +
			"individually. Normal alerting has resumed. [{{.Timestamp}}]",
	},
}

// Renderer turns NotificationEvents into message titles and bodies using Go
// text/template. Built-in defaults are compiled once; per-notification
// overrides are compiled on demand and cached. A broken override never loses
// an alert: rendering falls back to the default template and logs the error.
type Renderer struct {
	log *slog.Logger

	mu       sync.Mutex
	compiled map[string]*template.Template // cache, keyed by template source
}

// NewRenderer builds a Renderer.
func NewRenderer(logger *slog.Logger) *Renderer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Renderer{log: logger, compiled: make(map[string]*template.Template)}
}

// Render produces the title and body for an event, honouring the
// notification's per-kind overrides (nil or missing entries use defaults).
func (r *Renderer) Render(ev domain.NotificationEvent, overrides map[string]MessageTemplate) (title, body string) {
	def := defaultTemplates[ev.Kind]
	if def.Title == "" && def.Body == "" {
		// Unknown kind — degrade gracefully.
		def = MessageTemplate{
			Title: "[Statwarden] {{.MonitorName}}",
			Body:  "{{.MonitorName}}: {{.Message}} [{{.Timestamp}}]",
		}
	}
	ov := overrides[string(ev.Kind)]
	data := buildTemplateData(ev)

	title = r.renderOne(ov.Title, def.Title, data)
	body = r.renderOne(ov.Body, def.Body, data)
	return title, body
}

// renderOne renders the override source, falling back to the default source
// on any parse or execution error.
func (r *Renderer) renderOne(override, fallback string, data TemplateData) string {
	if override != "" {
		out, err := r.execute(override, data)
		if err == nil {
			return out
		}
		r.log.Warn("notification template override failed, using default", "error", err)
	}
	out, err := r.execute(fallback, data)
	if err != nil {
		// Defaults are tested; this is a programmer error. Degrade to a
		// bare string rather than dropping the alert.
		r.log.Error("default notification template failed", "error", err)
		return strings.TrimSpace(data.MonitorName + " " + data.Status + " " + data.Message)
	}
	return out
}

// execute compiles (with caching) and runs one template source.
func (r *Renderer) execute(src string, data TemplateData) (string, error) {
	tmpl, err := r.compile(src)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return sb.String(), nil
}

func (r *Renderer) compile(src string) (*template.Template, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tmpl, ok := r.compiled[src]; ok {
		return tmpl, nil
	}
	tmpl, err := template.New("message").Option("missingkey=zero").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	r.compiled[src] = tmpl
	return tmpl, nil
}

// buildTemplateData flattens an event into the template variable set.
func buildTemplateData(ev domain.NotificationEvent) TemplateData {
	data := TemplateData{
		Kind:              string(ev.Kind),
		MonitorName:       ev.MonitorName,
		MonitorURL:        ev.MonitorURL,
		Status:            string(ev.Status),
		Message:           ev.Message,
		Timestamp:         formatTime(ev.At),
		Reminder:          ev.Reminder,
		Escalation:        ev.Escalation,
		CertDaysRemaining: ev.CertDaysRemaining,
	}
	if ev.Latency != nil {
		data.Latency = ev.Latency.Round(time.Millisecond).String()
	}
	if ev.DownSince != nil {
		data.DownSince = formatTime(*ev.DownSince)
		if !ev.At.Before(*ev.DownSince) {
			data.DownDuration = ev.At.Sub(*ev.DownSince).Round(time.Second).String()
		}
	}
	if !ev.CertExpiresAt.IsZero() {
		data.CertExpiresAt = formatTime(ev.CertExpiresAt)
	}
	if ev.Anomaly != nil {
		data.AnomalyBaseline = ev.Anomaly.Baseline.Round(time.Millisecond).String()
		data.AnomalyDeviations = ev.Anomaly.Deviations
		// A zero-variance baseline scores +Inf, which would render as "+Inf σ".
		// Suppressing it loses nothing: the latency and the baseline are both in
		// the message and say the same thing more usefully.
		data.DeviationsKnown = !math.IsInf(ev.Anomaly.Deviations, 0) && !math.IsNaN(ev.Anomaly.Deviations)
		data.AnomalyRecovered = ev.Anomaly.Cleared
	}
	if ev.Flapping != nil {
		data.FlapChanges = ev.Flapping.Changes
		data.FlapWindow = ev.Flapping.WindowText()
	}
	for _, item := range ev.Items {
		data.Items = append(data.Items, buildTemplateData(item))
	}
	return data
}

// formatTime renders timestamps consistently in UTC.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
