// Package providers_test holds the cross-provider invariants — the rules that
// have to hold for all nineteen of them and that no single provider's own test
// can see.
package providers_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exempt lists http.Client literals that deliberately do not go through
// httpx.NotifierClient, with the reason.
var exempt = map[string]string{
	"webhook/webhook.go": "builds its client per send because the strip list is " +
		"per config, and does apply httpx.StripCredentialsOnRedirect itself",
}

// TestEveryProviderClientCarriesARedirectPolicy
//
// A provider POSTs a credential to a URL somebody configured. Left at nil,
// http.Client.CheckRedirect takes net/http's default: follow up to ten hops,
// stripping the sensitive headers only when the DOMAIN changes. An https→http hop
// on the SAME host is not a domain change, so a bearer token for a self-hosted
// Gotify, Matrix or ntfy instance was replayed over cleartext — the exact gap
// internal/httpx was created for, and which only the webhook provider was using.
//
// Eighteen providers each getting that right by hand is eighteen chances to get it
// wrong, and the eighteenth is whoever adds the next provider by copying an
// existing one. So the rule is mechanical: a bare http.Client literal in this
// tree is a finding unless it is listed above.
func TestEveryProviderClientCarriesARedirectPolicy(t *testing.T) {
	root := "."
	fset := token.NewFileSet()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel := filepath.ToSlash(path)
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Client" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
				return true
			}
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "CheckRedirect" {
						return true // sets its own policy
					}
				}
			}
			if why, ok := exempt[rel]; ok {
				t.Logf("%s is exempt — %s", rel, why)
				return true
			}
			t.Errorf("%s:%d builds an http.Client with no CheckRedirect. net/http then "+
				"follows redirects stripping credentials only on a DOMAIN change, so an "+
				"https→http hop on the same host replays the token in the clear. Use "+
				"httpx.NotifierClient(timeout, extraCredentialHeaders...), or set "+
				"CheckRedirect yourself and add %q to exempt saying why.",
				rel, fset.Position(lit.Pos()).Line, rel)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
