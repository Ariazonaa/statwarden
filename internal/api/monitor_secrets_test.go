package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/monitor/registry"

	// Checkers self-register from init(), and there is no import barrel — the
	// blank-import list lives in cmd/statwarden/main.go. Mirrored here so the
	// registry lookups below see every type. A checker missing from BOTH
	// lists fails this test at the registry lookup rather than passing
	// silently, which is the failure mode that matters.
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/certcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/dnscheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/dockercheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/grpccheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/httpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/mongocheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/mqttcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/mysqlcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/pingcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/postgrescheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/pushcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/radiuscheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/redischeck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/snmpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/steamcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/tcpcheck"
	_ "github.com/Ariazonaa/statwarden/internal/monitor/types/websocketcheck"
)

// typesDir is the source tree this test reads. It reads source rather than
// reflecting over types because registry.Checker exposes no config schema —
// Validate takes raw JSON — so the struct definitions are the only place the
// field names exist.
const typesDir = "../monitor/types"

// reviewedNonSecrets are config fields that trip the heuristic below and have
// been looked at and found not to be credentials. Adding a name here is a
// deliberate statement, which is the point: the alternative is a heuristic
// loose enough to be ignored.
var reviewedNonSecrets = map[string]string{
	"keyword":             "the string a response body is searched for",
	"keyword_absent":      "the string a response body must NOT contain",
	"auth_method":         "which auth scheme to use (none/basic/bearer), not the credential",
	"auth_protocol":       "the SNMPv3 hash algorithm name (MD5/SHA/…), not the password",
	"priv_protocol":       "the SNMPv3 cipher name (DES/AES/…), not the password",
	"tls_ca":              "a CA bundle: public by construction",
	"tls_cert":            "the public half of an mTLS pair",
	"tls_client_cert_pem": "the public half of an mTLS pair",
	"security_level":      "the SNMPv3 level name (noAuthNoPriv/…)",
	"basic_username":      "a username; masking it would hide which account a check uses",
	"username":            "a username; see basic_username",
	"user":                "a username; see basic_username",
	"client_id":           "an MQTT client identifier, not a credential",
}

// looksSensitive is deliberately broad. A false positive costs one line in
// reviewedNonSecrets; a false negative ships a credential to every viewer.
func looksSensitive(jsonName string) bool {
	for _, frag := range []string{
		"password", "passwd", "secret", "token", "credential",
		"community", "private", "key", "auth", "proxy", "dsn",
	} {
		if strings.Contains(jsonName, frag) {
			return true
		}
	}
	return false
}

// TestEveryCredentialFieldIsDeclaredSecret guards the redaction contract.
//
// Read responses decrypt a monitor's stored config and then mask the fields the
// checker names in Secrets(). A checker that names none gets no masking at all
// and is not a compile error, a vet finding or a test failure anywhere else —
// it is simply a config handed out whole. That is how dockercheck came to serve
// tls_key, the PRIVATE half of an mTLS pair that authenticates to a Docker
// daemon, to any authenticated viewer or read-only API key; and how httpcheck
// came to serve proxy_url, which the validator explicitly allows to carry
// user:password@host.
//
// Both were found by reading. This test is so the next one is not.
func TestEveryCredentialFieldIsDeclaredSecret(t *testing.T) {
	entries, err := os.ReadDir(typesDir)
	if err != nil {
		t.Fatalf("read %s: %v", typesDir, err)
	}

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkg := e.Name()
		fields, typeName := configFields(t, filepath.Join(typesDir, pkg))
		if len(fields) == 0 {
			continue // no JSON config of its own (e.g. the "all" import barrel)
		}
		checked++

		declared := declaredSecrets(t, pkg, typeName)
		for _, f := range fields {
			if !looksSensitive(f) || slices.Contains(declared, f) {
				continue
			}
			if why, reviewed := reviewedNonSecrets[f]; reviewed {
				t.Logf("%s: %q not masked — %s", pkg, f, why)
				continue
			}
			t.Errorf("%s: config field %q looks like a credential but is not in "+
				"Secrets() %v, so every read of such a monitor returns it in the clear. "+
				"Add it to Secrets(), or to reviewedNonSecrets with the reason it is safe.",
				pkg, f, declared)
		}
	}
	if checked < 10 {
		t.Fatalf("only inspected %d checker packages; the source walk is broken "+
			"and this test is passing vacuously", checked)
	}
}

// configFields returns the json tag names declared in a checker package, plus
// the registry type name it registers.
func configFields(t *testing.T, dir string) (fields []string, typeName string) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			// Field tags.
			if field, ok := n.(*ast.Field); ok && field.Tag != nil {
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					return true
				}
				name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
				if name != "" && name != "-" {
					fields = append(fields, name)
				}
			}
			// Every checker declares its registry name as `const TypeName`,
			// which Type() then returns.
			if vs, ok := n.(*ast.ValueSpec); ok {
				for i, name := range vs.Names {
					if name.Name != "TypeName" || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							typeName = v
						}
					}
				}
			}
			return true
		})
	}
	slices.Sort(fields)
	return slices.Compact(fields), typeName
}

// declaredSecrets asks the live registry, so the test measures what the API
// actually consults rather than re-reading the source it just parsed.
func declaredSecrets(t *testing.T, pkg, typeName string) []string {
	t.Helper()
	if typeName == "" {
		t.Fatalf("%s: could not find its `const TypeName`, so this test cannot "+
			"reach its Secrets() and would pass vacuously", pkg)
	}
	c, ok := registry.Default.Get(typeName)
	if !ok {
		t.Fatalf("checker %q is not registered; the Type() literal or the blank "+
			"import list is out of step", typeName)
	}
	sc, ok := c.(secretConfig)
	if !ok {
		return nil
	}
	return sc.Secrets()
}
