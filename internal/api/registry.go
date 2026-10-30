package api

import (
	"net/http"

	monreg "github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// monitorTypeInfo describes one registered monitor type for the dynamic
// config-form UI.
type monitorTypeInfo struct {
	Type    string `json:"type"`
	Passive bool   `json:"passive"`
}

// handleMonitorTypes lists the registered monitor types so the frontend can
// render a "new monitor" picker. Passive (push) types are flagged because they
// are configured, not probed.
func (s *server) handleMonitorTypes(w http.ResponseWriter, _ *http.Request) {
	names := s.monReg.List()
	out := make([]monitorTypeInfo, 0, len(names))
	for _, name := range names {
		info := monitorTypeInfo{Type: name}
		if c, ok := s.monReg.Get(name); ok {
			if _, passive := c.(monreg.PassiveChecker); passive {
				info.Passive = true
			}
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, out)
}

// notificationTypeInfo describes one registered notification provider.
type notificationTypeInfo struct {
	Type string `json:"type"`
}

// handleNotificationTypes lists the registered notification providers so the
// frontend can render the channel-config forms. Per-field metadata is a future
// enhancement (providers would need to expose a schema); today the type name is
// enough for the SPA to select the right hand-written form.
func (s *server) handleNotificationTypes(w http.ResponseWriter, _ *http.Request) {
	names := s.notifyReg.List()
	out := make([]notificationTypeInfo, 0, len(names))
	for _, name := range names {
		out = append(out, notificationTypeInfo{Type: name})
	}
	writeJSON(w, http.StatusOK, out)
}
