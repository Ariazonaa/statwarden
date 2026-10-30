package migrations

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	// checkRe finds the event_kind CHECK list in a migration. The LAST match in
	// migration order is the constraint a live database actually ends up with.
	checkRe  = regexp.MustCompile(`(?is)CHECK\s*\(\s*event_kind\s+IN\s*\(([^)]*)\)`)
	quotedRe = regexp.MustCompile(`'([^']*)'`)
)

// TestDeliveryEventKindsMatchTheSchema keeps domain.NotificationEventKind and the
// notification_deliveries CHECK constraint in step.
//
// They drifted once already, in the direction that hides itself. The
// response-time anomaly work added two kinds in Go and did not widen the
// constraint, so every anomaly delivery failed its audit-log INSERT — and the
// alert still went out, because RecordDelivery runs after the provider Send. So
// an operator saw the message, the log kept no row, and the dispatcher logged an
// error per delivery.
//
// Nothing else can catch this. The fast suite runs against MemStore, which has no
// constraints at all (CLAUDE.md), so a kind the database rejects is invisible
// until it reaches production. Both sides are therefore parsed from source: adding
// a constant without widening the constraint fails here, and so does widening the
// constraint for a kind that does not exist.
func TestDeliveryEventKindsMatchTheSchema(t *testing.T) {
	fromSQL := eventKindsFromMigrations(t)
	fromGo := eventKindsFromDomain(t)

	if len(fromSQL) == 0 {
		t.Fatal("found no event_kind CHECK in the migrations; this test needs updating")
	}
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Errorf("event kinds disagree:\n  schema (migrations): %v\n  domain (Go constants): %v\n"+
			"A kind only Go knows about is rejected by the database on every "+
			"delivery; a kind only the schema knows about is dead. Widen both, in "+
			"the same commit.", fromSQL, fromGo)
	}
}

// eventKindsFromMigrations returns the kinds the final CHECK constraint allows.
func eventKindsFromMigrations(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(files, "*.up.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(names) // numeric prefixes sort in application order

	var last string
	for _, name := range names {
		raw, rerr := files.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		// Skip the commentary in 0015, which quotes the query rather than the
		// constraint: only a real CHECK clause counts.
		for _, m := range checkRe.FindAllStringSubmatch(string(raw), -1) {
			last = m[1]
		}
	}
	if last == "" {
		return nil
	}
	var out []string
	for _, m := range quotedRe.FindAllStringSubmatch(last, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// eventKindsFromDomain returns the string values of every
// domain.NotificationEventKind constant.
func eventKindsFromDomain(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "domain", "notification.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		ident, ok := spec.Type.(*ast.Ident)
		if !ok || ident.Name != "NotificationEventKind" {
			return true
		}
		for _, v := range spec.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				continue
			}
			out = append(out, s)
		}
		return true
	})
	sort.Strings(out)
	return out
}
