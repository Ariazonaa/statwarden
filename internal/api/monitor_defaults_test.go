package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// newChannel creates a notification channel and returns its id.
func newChannel(t *testing.T, h *harness, admin *session, name string, isDefault, active bool) int64 {
	t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/notifications",
		notificationRequest{Name: name, Type: "fake", IsDefault: isDefault, Active: &active}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create channel %q = %d, body %s", name, rec.Code, rec.Body.String())
	}
	var out struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &out)
	return out.ID
}

// createMonitorRaw posts a monitor from raw JSON, so a test can control whether
// notification_ids is ABSENT, explicitly [], or a list. The typed
// monitorRequest cannot express the first two apart.
func createMonitorRaw(t *testing.T, h *harness, admin *session, body string) (int64, []int64) {
	t.Helper()
	rec := h.requestRaw(http.MethodPost, "/api/v1/monitors", body, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID              int64   `json:"id"`
		NotificationIDs []int64 `json:"notification_ids"`
	}
	mustJSON(t, rec, &out)
	return out.ID, out.NotificationIDs
}

// TestDefaultChannelsAttachToNewMonitors covers the whole rule, and the
// distinction the rule rests on.
//
// notify.Store has documented is_default as "channels that are auto-attached to
// new monitors" since the flag existed, and nothing implemented it: the column
// round-tripped through the API and changed no behaviour anywhere.
func TestDefaultChannelsAttachToNewMonitors(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	def := newChannel(t, h, admin, "on-call", true, true)
	plain := newChannel(t, h, admin, "weekly-digest", false, true)
	offDefault := newChannel(t, h, admin, "retired-pager", true, false)

	t.Run("an omitted field gets the active defaults", func(t *testing.T) {
		_, got := createMonitorRaw(t, h, admin,
			`{"name":"omitted","type":"fake","interval_seconds":60}`)
		if len(got) != 1 || got[0] != def {
			t.Fatalf("notification_ids = %v, want exactly [%d]", got, def)
		}
	})

	t.Run("an inactive default is not attached", func(t *testing.T) {
		_, got := createMonitorRaw(t, h, admin,
			`{"name":"inactive-default","type":"fake","interval_seconds":60}`)
		for _, id := range got {
			if id == offDefault {
				t.Fatalf("a deactivated default channel was attached: %v", got)
			}
		}
	})

	// The load-bearing case. The web form always sends notification_ids, so if
	// an empty list also meant "use the defaults", an operator could never
	// create a monitor without channels — deselecting every box would silently
	// put them back, with no way to express the choice at all.
	t.Run("an explicit empty list means no channels", func(t *testing.T) {
		_, got := createMonitorRaw(t, h, admin,
			`{"name":"deliberately-silent","type":"fake","interval_seconds":60,"notification_ids":[]}`)
		if len(got) != 0 {
			t.Fatalf("notification_ids = %v, want none", got)
		}
	})

	t.Run("an explicit list wins and defaults are not added to it", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"name": "explicit", "type": "fake", "interval_seconds": 60,
			"notification_ids": []int64{plain},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, got := createMonitorRaw(t, h, admin, string(body))
		if len(got) != 1 || got[0] != plain {
			t.Fatalf("notification_ids = %v, want exactly [%d] — defaults must not be unioned in", got, plain)
		}
	})

	t.Run("no defaults configured is not an error", func(t *testing.T) {
		h2 := newHarness(t)
		admin2 := h2.setupAdmin()
		newChannel(t, h2, admin2, "not-default", false, true)
		_, got := createMonitorRaw(t, h2, admin2,
			`{"name":"nothing-to-attach","type":"fake","interval_seconds":60}`)
		if len(got) != 0 {
			t.Fatalf("notification_ids = %v, want none", got)
		}
	})
}

// TestDefaultChannelsDoNotApplyOnUpdate pins that this is a creation-time rule.
// An update is a full replace, so re-adding defaults there would make it
// impossible to ever detach one.
func TestDefaultChannelsDoNotApplyOnUpdate(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	def := newChannel(t, h, admin, "on-call", true, true)

	id, got := createMonitorRaw(t, h, admin, `{"name":"m","type":"fake","interval_seconds":60}`)
	if len(got) != 1 || got[0] != def {
		t.Fatalf("create did not attach the default: %v", got)
	}

	rec := h.requestRaw(http.MethodPut, "/api/v1/monitors/"+itoa64(id),
		`{"name":"m","type":"fake","interval_seconds":60,"notification_ids":[]}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		NotificationIDs []int64 `json:"notification_ids"`
	}
	mustJSON(t, rec, &updated)
	if len(updated.NotificationIDs) != 0 {
		t.Fatalf("update re-attached the default channel: %v — detaching would be impossible",
			updated.NotificationIDs)
	}
}
