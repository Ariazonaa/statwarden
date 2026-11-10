package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
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
// docker-compose.yml together. Everything under .github is in scope for the
// same reason: the workflows, dependabot.yml and the issue forms are only ever
// read by GitHub, and release.yml least of all — it runs on a tag, so a syntax
// error there stays invisible until someone is cutting a release.
//
// Parsing is the whole assertion. It is not a schema check: docker compose and
// GitHub each validate their own semantics, and duplicating that here would rot.
func TestShippedYAMLParses(t *testing.T) {
	root := repoRoot(t)
	files := shippedYAML(t, root)

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

// shippedYAML returns every YAML file under root that TestShippedYAMLParses covers.
func shippedYAML(t *testing.T, root string) []string {
	t.Helper()

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

	// Walked rather than listed so a new workflow, issue form or bot config is
	// covered the day it lands, whichever of the two YAML extensions it uses.
	// No workflow found is a hard failure: silently covering nothing is exactly
	// the hole this test exists to close.
	workflows := 0
	err = filepath.WalkDir(filepath.Join(root, ".github"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".yml", ".yaml":
			files = append(files, path)
			if filepath.Base(filepath.Dir(path)) == "workflows" {
				workflows++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk .github: %v", err)
	}
	if workflows == 0 {
		t.Fatal("no YAML file found under .github/workflows — the layout moved and this test would cover almost nothing")
	}
	return files
}

// The file set is asserted on a synthetic tree, so the walk is proven to reach
// every kind of file under .github without depending on what the real
// repository happens to contain today.
func TestShippedYAMLCoversGitHubDir(t *testing.T) {
	root := t.TempDir()
	want := []string{
		"docker-compose.yml",
		"docker-compose.dev.yml",
		".github/dependabot.yml",
		".github/workflows/ci.yml",
		".github/workflows/release.yaml",
		".github/ISSUE_TEMPLATE/bug_report.yml",
		".github/ISSUE_TEMPLATE/config.yaml",
	}
	skip := []string{
		".github/pull_request_template.md",
		"web/node_modules/pkg/action.yml",
	}
	for _, rel := range append(append([]string{}, want...), skip...) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("a: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for _, path := range shippedYAML(t, root) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, filepath.ToSlash(rel))
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("covered files:\n got  %q\n want %q", got, want)
	}
}
