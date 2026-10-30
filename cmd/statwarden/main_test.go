package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
