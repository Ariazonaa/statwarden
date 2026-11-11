package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// parseMain parses main.go for the source-level wiring tests below: run()
// needs a database, so the composition root cannot be exercised directly.
func parseMain(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return file
}

// TestHTTPServerBoundsHeaderSize pins MaxHeaderBytes on the server. Left at
// zero, net/http allows 1 MiB of headers per request, and every unauthenticated
// endpoint — the push heartbeat URL first among them — will buffer that much
// before a handler can refuse anything.
func TestHTTPServerBoundsHeaderSize(t *testing.T) {
	found, set := false, false
	ast.Inspect(parseMain(t), func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Server" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
			return true
		}
		found = true
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "MaxHeaderBytes" {
					set = true
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("no http.Server literal in main.go; this test needs updating")
	}
	if !set {
		t.Fatal("the http.Server sets no MaxHeaderBytes, so every request may carry 1 MiB of headers")
	}
	if maxHeaderBytes <= 0 || maxHeaderBytes > 1<<20 {
		t.Fatalf("maxHeaderBytes = %d, want a positive bound below net/http's 1 MiB default", maxHeaderBytes)
	}
}

// TestShutdownStopsHTTPBeforeTheDispatcher pins the shutdown order.
//
// The push endpoint is an HTTP handler that emits state changes into the
// dispatcher. With the dispatcher stopped first, a push that arrived during the
// HTTP drain — a recovery, typically, from a cron job that just came back —
// was accepted with 200 and its alert silently dropped. The server has to stop
// taking requests before anything they feed is stopped.
func TestShutdownStopsHTTPBeforeTheDispatcher(t *testing.T) {
	pos := map[string]token.Pos{}
	ast.Inspect(parseMain(t), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		key := recv.Name + "." + sel.Sel.Name
		if _, seen := pos[key]; !seen {
			pos[key] = call.Pos()
		}
		return true
	})
	for _, k := range []string{"srv.Shutdown", "eng.Stop", "dispatcher.Stop"} {
		if pos[k] == token.NoPos {
			t.Fatalf("no %s call in main.go; this test needs updating", k)
		}
	}
	if pos["srv.Shutdown"] > pos["dispatcher.Stop"] {
		t.Error("dispatcher.Stop runs before srv.Shutdown: push heartbeats received during the HTTP drain lose their alerts")
	}
	if pos["srv.Shutdown"] > pos["eng.Stop"] {
		t.Error("eng.Stop runs before srv.Shutdown: a push monitor's transition during the drain reaches a stopped engine")
	}
	if pos["eng.Stop"] > pos["dispatcher.Stop"] {
		t.Error("dispatcher.Stop runs before eng.Stop: the engine's last transitions have nobody to deliver them")
	}
}

// TestMetricsTokenCacheRotatesWithinTheTTL pins the per-request token read.
// The token used to be read once at startup, so a rotated (leaked) token kept
// working until a restart; now a change is picked up within one TTL, while a
// scrape every few seconds still costs at most one settings read per TTL.
func TestMetricsTokenCacheRotatesWithinTheTTL(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	stored, reads := "first", 0
	var fail error
	c := newCachedToken(func(context.Context) (string, error) {
		reads++
		return stored, fail
	}, 15*time.Second, quietLogger())
	c.now = func() time.Time { return now }

	if got := c.get(context.Background()); got != "first" {
		t.Fatalf("first read = %q", got)
	}
	stored = "second"
	if got := c.get(context.Background()); got != "first" || reads != 1 {
		t.Fatalf("within the TTL: got %q after %d reads, want the cached value from 1 read", got, reads)
	}
	now = now.Add(16 * time.Second)
	if got := c.get(context.Background()); got != "second" {
		t.Fatalf("after the TTL: got %q, want the rotated token", got)
	}

	// An unreadable token closes the endpoint rather than keeping a stale one
	// open — the safe direction — and is cached too, so a broken database is
	// not asked again on every scrape.
	fail = errors.New("database down")
	now = now.Add(16 * time.Second)
	if got := c.get(context.Background()); got != "" {
		t.Fatalf("on a read error: got %q, want \"\" (closed)", got)
	}
	before := reads
	c.get(context.Background())
	if reads != before {
		t.Fatal("a failed read was retried within the TTL")
	}
}

// Detection off must stay entirely inert, which is what the engine's nil guard
// relies on.
func TestNoAnomalyDetectorWhenDisabled(t *testing.T) {
	if d := newAnomalyDetector(config.AnomalyConfig{Enabled: false}, quietLogger()); d != nil {
		t.Fatal("a disabled configuration produced a detector")
	}
}

// Enabling detection must produce a detector that carries the CONFIGURED
// thresholds, not the package defaults.
//
// Consecutive is what makes this an assertion rather than a nil check: the
// default is 3, so a detector built with 1 raises on the first anomalous check
// and a detector built from a dropped config does not. That distinguishes "wired"
// from "wired but ignoring the settings".
func TestAnomalyDetectorUsesTheConfiguredThresholds(t *testing.T) {
	d := newAnomalyDetector(config.AnomalyConfig{
		Enabled:     true,
		Sensitivity: 2,
		MinSamples:  2,
		Consecutive: 1,
	}, quietLogger())
	if d == nil {
		t.Fatal("an enabled configuration produced no detector — the feature is inert")
	}

	// Warm the baseline up to MinSamples, then hand it an obvious spike.
	d.Observe(1, float64(100))
	d.Observe(1, float64(100))
	got := d.Observe(1, float64(100_000))
	if !got.Raised {
		t.Fatalf("a 1000x spike did not raise with Consecutive=1: %+v — the "+
			"configured thresholds did not reach the detector", got)
	}
}

// The composition root must actually hand the detector to the engine.
//
// This is the defect itself: every layer of the feature was present and tested,
// and main.go's engine.Options literal simply had no Anomaly field, so nothing
// ran. No unit test can see that — run() needs a database — so it is asserted
// against the source, in the same spirit as internal/safego's enforcement tests.
func TestEngineOptionsWireTheAnomalyDetector(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found, wired := false, false
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Options" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "engine" {
			return true
		}
		found = true
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Anomaly" {
				wired = true
			}
		}
		return true
	})

	if !found {
		t.Fatal("no engine.Options literal in main.go; this test needs updating")
	}
	if !wired {
		t.Fatal("main.go builds the engine without an Anomaly detector, so " +
			"ANOMALY_DETECTION_ENABLED does nothing — the config is parsed and " +
			"validated, .env.example and the README promise alerts, and no " +
			"latency anomaly is ever detected")
	}
}
