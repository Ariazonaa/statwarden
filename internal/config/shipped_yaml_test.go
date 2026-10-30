package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot resolves the repository root from this package's directory, and
// proves it found the right place instead of trusting the relative hop.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod under %s — this package moved and the hop above needs updating: %v", root, err)
	}
	return root
}

// Every YAML file this repository ships has to parse.
//
// Nothing in the build, the test suite or CI read them otherwise, which is how
// a `: ` inside an unquoted ${SESSION_SECRET:?…} message — a mapping indicator
// to YAML — sat in docker-compose.yml for 81 commits. Every docker compose
// subcommand refused to run:
//
//	go-yaml load error in scanner at L37.C82:
//	mapping values are not allowed in this context
//
// while all four CI jobs stayed green, so the primary install path in
// docs/deployment.md was broken for operators that entire time.
//
// This lives in internal/config because the package already owns the other half
// of that contract: a new knob touches config.go, .env.example and
// docker-compose.yml together. The workflows are in scope for the same reason,
// and release.yml is the one that needs it most — it only ever runs on a tag,
// so a syntax error there stays invisible until someone is cutting a release.
//
// Parsing is the whole assertion. It is not a schema check: docker compose and
// Gitea each validate their own semantics, and duplicating that here would rot.
func TestShippedYAMLParses(t *testing.T) {
	root := repoRoot(t)

	// Globbed, not listed. The two compose files used to be named here, and
	// docker-compose.build.yml showed what that costs: a third one lands and the
	// gate covers everything except the file nobody remembered to add — which is
	// the same failure this test was written to prevent, one level up.
	files, err := filepath.Glob(filepath.Join(root, "docker-compose*.yml"))
	if err != nil {
		t.Fatalf("glob compose files: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("docker-compose*.yml matched %d files — the layout moved and this test would cover almost nothing", len(files))
	}

	// Globbed rather than listed so a new workflow is covered the day it lands.
	// An empty glob is a hard failure: silently covering nothing is exactly the
	// hole this test exists to close.
	workflows, err := filepath.Glob(filepath.Join(root, ".gitea", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(workflows) == 0 {
		t.Fatal("no files matched .gitea/workflows/*.yml — the layout moved and this test would cover nothing")
	}
	files = append(files, workflows...)

	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var doc any
			if err := yaml.Unmarshal(b, &doc); err != nil {
				t.Fatalf("does not parse as YAML: %v", err)
			}
		})
	}
}
