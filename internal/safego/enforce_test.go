package safego_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// exempt lists the `go` statements that deliberately do not use safego, with
// the reason. An entry here is a decision someone made on purpose; anything
// else is an oversight, which is the distinction this test exists to keep.
var exempt = map[string]string{
	"internal/monitor/engine/engine.go:e.runCheck": "runCheck contains its own panics and has cleanup " +
		"that must run either way; recovering above it would clear the recover but wedge the job",
}

// TestNoBareGoroutines
//
// An unrecovered panic in ANY goroutine ends the process. Two of them did
// exactly that before this package existed — a checker parsing a malformed
// response, and a notification provider — and the remedy is only as good as its
// coverage: one goroutine added later without containment restores the original
// failure, and nothing would say so.
//
// So the rule is mechanical rather than remembered. Tests are exempt: a panic
// there is a failing test, which is the point.
func TestNoBareGoroutines(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "sqlcgen":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if strings.HasPrefix(rel, "internal/safego/") {
			return nil // the implementation itself
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			stmt, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			// safego.Go(...) is the sanctioned form.
			if sel, ok := stmt.Call.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "safego" {
					return true
				}
			}
			called := describe(stmt.Call.Fun)
			if why, ok := exempt[rel+":"+called]; ok {
				t.Logf("%s: `go %s` is exempt — %s", rel, called, why)
				return true
			}
			t.Errorf("%s:%d: `go %s` starts a goroutine without panic containment. "+
				"An unrecovered panic there ends the process. Use safego.Go, or "+
				"contain the panic inside the function and add an entry to `exempt` "+
				"saying why.", rel, fset.Position(stmt.Pos()).Line, called)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// loopExempt lists periodic loops that deliberately do not contain their own
// iterations, with the reason. Keyed by file and enclosing function, like
// `exempt` above, so it survives edits that move lines around.
var loopExempt = map[string]string{
	"internal/ws/client.go:writePump": "a panic unwinds through the deferred c.close(), " +
		"which tears the connection down — one dead socket is the correct unit of " +
		"failure for a per-connection pump, and there is nothing left to iterate",
	"internal/ws/client.go:authPump": "same: the deferred c.close() makes a panic drop " +
		"the connection, which is fail-closed and matches what this pump does whenever " +
		"it cannot prove the session",
}

// TestPeriodicLoopsContainTheirIterations
//
// safego.Go recovers a panic and then lets the goroutine END. Wrapping a loop in
// it therefore buys a log line and nothing else: the loop is gone for the life of
// the process, while the rest of the service still reports itself healthy. The
// package doc says the unit should be one iteration rather than the loop around
// it, and TestNoBareGoroutines above cannot see the difference — it only checks
// that a goroutine is wrapped at all, which is how three ticker loops (the push
// watchdog, the rollup job and the certificate sweeper) came to be wrapped at the
// wrong level and stay that way.
//
// A ticker receive inside an unconditioned for-select is a precise marker for
// "periodic background job", so the rule is mechanical here too.
func TestPeriodicLoopsContainTheirIterations(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "sqlcgen", "archive":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if strings.HasPrefix(rel, "internal/safego/") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		// Walked per function so a finding can name the function it is in, which
		// is what makes an exemption key stable across unrelated edits.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				loop, ok := n.(*ast.ForStmt)
				if !ok || loop.Cond != nil || loop.Body == nil {
					return true
				}
				if !hasTickerReceive(loop.Body) || containsSafegoDo(loop.Body) {
					return true
				}
				key := rel + ":" + fn.Name.Name
				if why, ok := loopExempt[key]; ok {
					t.Logf("%s is exempt — %s", key, why)
					return true
				}
				t.Errorf("%s:%d: %s runs a periodic loop with no safego.Do inside its "+
					"body. safego.Go around the loop recovers ABOVE it, so one panic ends "+
					"the goroutine and the job never runs again — silently. Contain each "+
					"iteration, or add %q to loopExempt saying why.",
					rel, fset.Position(loop.Pos()).Line, fn.Name.Name, key)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// hasTickerReceive reports whether the block selects on a ticker channel — the
// marker for "this loop runs on a schedule".
func hasTickerReceive(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectStmt)
		if !ok {
			return true
		}
		for _, cl := range sel.Body.List {
			comm, ok := cl.(*ast.CommClause)
			if !ok || comm.Comm == nil {
				continue
			}
			ast.Inspect(comm.Comm, func(m ast.Node) bool {
				unary, ok := m.(*ast.UnaryExpr)
				if !ok || unary.Op != token.ARROW {
					return true
				}
				if s, ok := unary.X.(*ast.SelectorExpr); ok && s.Sel.Name == "C" {
					found = true
				}
				return true
			})
		}
		return true
	})
	return found
}

// containsSafegoDo reports whether the block calls safego.Do anywhere inside it,
// including within nested function literals.
func containsSafegoDo(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Do" {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "safego" {
				found = true
			}
		}
		return true
	})
	return found
}

// describe renders the called expression compactly enough for an exempt key.
func describe(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return describe(v.X) + "." + v.Sel.Name
	case *ast.FuncLit:
		return "func()"
	default:
		return "?"
	}
}
