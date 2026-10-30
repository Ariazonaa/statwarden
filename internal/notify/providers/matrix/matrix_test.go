package matrix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
)

func TestSelfRegisters(t *testing.T) {
	if _, ok := registry.Get("matrix"); !ok {
		t.Fatal("matrix provider did not self-register")
	}
}

func TestValidate(t *testing.T) {
	n := New()
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok", `{"homeserver_url":"https://matrix.example.org","access_token":"tok","room_id":"!r:example.org"}`, false},
		{"missing homeserver", `{"access_token":"t","room_id":"!r:x"}`, true},
		{"bad scheme", `{"homeserver_url":"ftp://x","access_token":"t","room_id":"!r:x"}`, true},
		{"missing token", `{"homeserver_url":"https://m","room_id":"!r:x"}`, true},
		{"missing room", `{"homeserver_url":"https://m","access_token":"t"}`, true},
		{"broken json", `{"homeserver_url":`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := n.Validate(json.RawMessage(tt.cfg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestSend(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType string
	var gotBody payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"homeserver_url":"` + srv.URL + `","access_token":"secrettok","room_id":"!room:example.org"}`)
	ev := domain.NotificationEvent{
		Kind: domain.EventKindDown, MonitorName: "API",
		MonitorURL: "https://api.example.com", Status: domain.StatusDown,
		Title: "API is DOWN", Body: "connection refused",
	}
	if err := n.Send(t.Context(), cfg, ev); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %q, want PUT", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotAuth != "Bearer secrettok" {
		t.Fatalf("auth = %q, want Bearer secrettok", gotAuth)
	}
	wantPrefix := "/_matrix/client/v3/rooms/" + "%21room:example.org" + "/send/m.room.message/"
	if !strings.HasPrefix(gotPath, "/_matrix/client/v3/rooms/") || !strings.Contains(gotPath, "/send/m.room.message/") {
		t.Fatalf("path = %q, want prefix %q", gotPath, wantPrefix)
	}
	if gotBody.MsgType != "m.text" {
		t.Fatalf("msgtype = %q", gotBody.MsgType)
	}
	if !strings.Contains(gotBody.Body, "API is DOWN") {
		t.Fatalf("body missing title: %q", gotBody.Body)
	}
	if !strings.Contains(gotBody.FormattedBody, "<strong>API is DOWN</strong>") {
		t.Fatalf("formatted_body missing: %q", gotBody.FormattedBody)
	}
}

func TestSendEscapesRoomID(t *testing.T) {
	var gotRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"homeserver_url":"` + srv.URL + `","access_token":"t","room_id":"!room:example.org"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "t"}); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if !strings.Contains(gotRawPath, "%21room:example.org") {
		t.Fatalf("room id not escaped in path: %q", gotRawPath)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := New()
	cfg := json.RawMessage(`{"homeserver_url":"` + srv.URL + `","access_token":"t","room_id":"!r:x"}`)
	if err := n.Send(t.Context(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest}); err == nil {
		t.Fatal("expected error on 403 response")
	}
}
