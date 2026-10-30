package dockercheck

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// fakeProbe builds a probe returning the given state and error.
func fakeProbe(st containerState, err error) func(context.Context, Config) (containerState, error) {
	return func(context.Context, Config) (containerState, error) { return st, err }
}

func TestCheckResultMapping(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		st         containerState
		probeErr   error
		wantStatus domain.MonitorStatus
	}{
		{
			"running is up",
			Config{Container: "web"},
			containerState{status: "running"}, nil,
			domain.StatusUp,
		},
		{
			"exited is down",
			Config{Container: "web"},
			containerState{status: "exited"}, nil,
			domain.StatusDown,
		},
		{
			"connection refused is down",
			Config{Container: "web"},
			containerState{}, errors.New("connection refused"),
			domain.StatusDown,
		},
		{
			"no such container is down",
			Config{Container: "ghost"},
			containerState{}, errors.New("no such container: ghost"),
			domain.StatusDown,
		},
		{
			"timeout is down",
			Config{Container: "web"},
			containerState{}, context.DeadlineExceeded,
			domain.StatusDown,
		},
		{
			"healthy required and healthy is up",
			Config{Container: "web", RequiredState: "healthy"},
			containerState{status: "running", health: "healthy"}, nil,
			domain.StatusUp,
		},
		{
			"healthy required but unhealthy is down",
			Config{Container: "web", RequiredState: "healthy"},
			containerState{status: "running", health: "unhealthy"}, nil,
			domain.StatusDown,
		},
		{
			"healthy required but no healthcheck is down",
			Config{Container: "web", RequiredState: "healthy"},
			containerState{status: "running", health: ""}, nil,
			domain.StatusDown,
		},
		{
			"running only ignores health",
			Config{Container: "web"},
			containerState{status: "running", health: "starting"}, nil,
			domain.StatusUp,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Checker{probe: fakeProbe(tt.st, tt.probeErr)}
			raw, err := json.Marshal(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ch.Check(context.Background(), raw)
			if err != nil {
				t.Fatalf("Check() unexpected error: %v", err)
			}
			if got.Status != tt.wantStatus {
				t.Fatalf("Check() status = %q (%q), want %q", got.Status, got.Message, tt.wantStatus)
			}
		})
	}
}

func TestCheckRejectsBadConfig(t *testing.T) {
	ch := &Checker{probe: fakeProbe(containerState{status: "running"}, nil)}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Check() with invalid config = nil error, want error")
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := registry.Get(TypeName); !ok {
		t.Fatalf("type %q not registered in Default registry", TypeName)
	}
}

// inspectHandler serves a canned /containers/{id}/json response emulating the
// Docker Engine API, so liveInspect can be exercised end to end.
func inspectHandler(status, health string, code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		body := map[string]any{"State": map[string]any{"Status": status}}
		if health != "" {
			body["State"].(map[string]any)["Health"] = map[string]any{"Status": health}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// TestLiveInspectTCP drives liveInspect against a loopback TCP server that
// speaks the Engine API — no docker daemon involved.
func TestLiveInspectTCP(t *testing.T) {
	srv := httptest.NewServer(inspectHandler("running", "healthy", http.StatusOK))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:port; rewrite the scheme to tcp:// so the
	// checker treats it as a remote Docker host.
	cfg := Config{Endpoint: "tcp://" + srv.Listener.Addr().String(), Container: "web", RequiredState: "healthy"}
	st, err := liveInspect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("liveInspect() error = %v", err)
	}
	if st.status != "running" || st.health != "healthy" {
		t.Fatalf("liveInspect() = %+v, want running/healthy", st)
	}
}

func TestLiveInspectNotFound(t *testing.T) {
	srv := httptest.NewServer(inspectHandler("", "", http.StatusNotFound))
	defer srv.Close()

	cfg := Config{Endpoint: "tcp://" + srv.Listener.Addr().String(), Container: "ghost"}
	_, err := liveInspect(context.Background(), cfg)
	if err == nil {
		t.Fatal("liveInspect() on 404 = nil error, want no-such-container")
	}
}

// TestLiveInspectUnixSocket drives liveInspect over a real unix-domain socket
// serving the Engine API, covering the local-socket transport.
func TestLiveInspectUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: inspectHandler("running", "", http.StatusOK)}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	// Address the socket with an opaque "unix:<path>" endpoint rather than the
	// authority form: a Windows temp dir is a drive-letter path, which url.Parse
	// reads as a port separator after the "unix://" authority. A POSIX path keeps
	// its leading slash here and still parses as a URL path.
	cfg := Config{Endpoint: "unix:" + filepath.ToSlash(sock), Container: "web"}
	st, err := liveInspect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("liveInspect() over unix socket error = %v", err)
	}
	if st.status != "running" {
		t.Fatalf("liveInspect() = %+v, want running", st)
	}
	_ = os.Remove(sock)
}
