package grpccheck

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// fakeProbe builds a probe returning the given status and error.
func fakeProbe(status string, err error) func(context.Context, Config) (string, error) {
	return func(context.Context, Config) (string, error) { return status, err }
}

func TestCheckResultMapping(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		status     string
		probeErr   error
		wantStatus domain.MonitorStatus
	}{
		{
			"serving is up",
			Config{Target: "svc:50051"},
			"SERVING", nil,
			domain.StatusUp,
		},
		{
			"not_serving fails default expectation",
			Config{Target: "svc:50051"},
			"NOT_SERVING", nil,
			domain.StatusDown,
		},
		{
			"keyword variant matches not_serving",
			Config{Target: "svc:50051", ExpectedStatus: "NOT_SERVING"},
			"NOT_SERVING", nil,
			domain.StatusUp,
		},
		{
			"connection refused is down",
			Config{Target: "svc:50051"},
			"", errors.New("connection refused"),
			domain.StatusDown,
		},
		{
			"timeout is down",
			Config{Target: "svc:50051"},
			"", context.DeadlineExceeded,
			domain.StatusDown,
		},
		{
			"service_unknown is down under default",
			Config{Target: "svc:50051", Service: "ghost"},
			"SERVICE_UNKNOWN", nil,
			domain.StatusDown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Checker{probe: fakeProbe(tt.status, tt.probeErr)}
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
	ch := &Checker{probe: fakeProbe("SERVING", nil)}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Check() with invalid config = nil error, want error")
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := registry.Get(TypeName); !ok {
		t.Fatalf("type %q not registered in Default registry", TypeName)
	}
}

// bufHealthServer starts an in-memory gRPC health server over bufconn and
// returns a client connection to it. No TCP or real network is used.
func bufHealthServer(t *testing.T, overall grpc_health_v1.HealthCheckResponse_ServingStatus, services map[string]grpc_health_v1.HealthCheckResponse_ServingStatus) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("", overall)
	for name, st := range services {
		hs.SetServingStatus(name, st)
	}
	grpc_health_v1.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestCheckHealthOverBufconn exercises the real Health/Check RPC against an
// in-memory server for both the overall and a named service.
func TestCheckHealthOverBufconn(t *testing.T) {
	conn := bufHealthServer(t, grpc_health_v1.HealthCheckResponse_SERVING, map[string]grpc_health_v1.HealthCheckResponse_ServingStatus{
		"pkg.Service": grpc_health_v1.HealthCheckResponse_NOT_SERVING,
	})

	got, err := checkHealth(context.Background(), conn, "")
	if err != nil {
		t.Fatalf("checkHealth(overall) error = %v", err)
	}
	if got != "SERVING" {
		t.Fatalf("checkHealth(overall) = %q, want SERVING", got)
	}

	got, err = checkHealth(context.Background(), conn, "pkg.Service")
	if err != nil {
		t.Fatalf("checkHealth(service) error = %v", err)
	}
	if got != "NOT_SERVING" {
		t.Fatalf("checkHealth(service) = %q, want NOT_SERVING", got)
	}
}
