// This test exists because MonitorCreateRequest (monitors.go) is a hand-copied
// duplicate of api.monitorRequest (internal/api/monitors.go), and the two can
// drift silently.
//
// The duplication is deliberate: `go list -deps ./internal/api` is ~275
// packages including chi, pgx and an image codec, none of which a CLI that
// speaks plain HTTP should link. So the DTO is redeclared here instead of
// imported.
//
// The cost of that choice is this failure mode: internal/api/respond.go's
// decodeJSON calls dec.DisallowUnknownFields(), so a json tag this CLI sends
// that the server does not know is NOT silently ignored — it is an
// unconditional HTTP 400 on every single create. And a tag renamed on the
// server side (interval_seconds -> interval, say) breaks this CLI at runtime
// with nothing in either build failing.
//
// The check is one-directional, matching internal/api/registry_contract_test.go:
// a server field this CLI does not expose is fine (the CLI is a subset by
// design); a CLI field the server does not accept is never fine.
//
// The server struct is read by parsing its source rather than by reflection,
// because monitorRequest is unexported and importing internal/api is the exact
// thing this design avoids.
//
// Please do not delete this test to make a change go green. If it fails, the
// fix is to reconcile the two structs.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const serverMonitorsFile = "../../internal/api/monitors.go"

func TestMonitorCreateRequestMatchesServerDTO(t *testing.T) {
	serverTags := parseStructJSONTags(t, serverMonitorsFile, "monitorRequest")
	if len(serverTags) == 0 {
		t.Fatalf("parsed zero json tags from %s type monitorRequest — the parser in this test is broken, "+
			"or the struct moved. Fix the parser rather than deleting the test.", serverMonitorsFile)
	}

	cliTags := reflectJSONTags(reflect.TypeOf(MonitorCreateRequest{}))
	if len(cliTags) == 0 {
		t.Fatal("parsed zero json tags from MonitorCreateRequest — the reflection in this test is broken")
	}

	for _, tag := range sortedKeys(cliTags) {
		if !serverTags[tag] {
			t.Errorf("MonitorCreateRequest sends json field %q, which api.monitorRequest does not declare.\n"+
				"The server decodes with DisallowUnknownFields, so this is a hard HTTP 400 on every create.\n"+
				"Fix: rename or remove the field in cmd/statwarden-cli/monitors.go, or add it to %s.",
				tag, serverMonitorsFile)
		}
	}
}

// parseStructJSONTags extracts the json tag names of the named struct type in
// the given Go source file.
func parseStructJSONTags(t *testing.T, path, typeName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.FromSlash(path), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	tags := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typeName {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			raw, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				continue
			}
			if name := tagName(reflect.StructTag(raw).Get("json")); name != "" {
				tags[name] = true
			}
		}
		return false
	})
	return tags
}

// reflectJSONTags extracts the json tag names of a struct type.
func reflectJSONTags(typ reflect.Type) map[string]bool {
	tags := map[string]bool{}
	for i := range typ.NumField() {
		if name := tagName(typ.Field(i).Tag.Get("json")); name != "" {
			tags[name] = true
		}
	}
	return tags
}

// tagName strips the options from a json struct tag, returning "" for an
// absent or explicitly skipped field.
func tagName(tag string) string {
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return ""
	}
	return name
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
