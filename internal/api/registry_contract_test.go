package api

// Cross-language guard for web/src/lib/api/registry.ts.
//
// The monitor/notification config forms are driven by field schemas AUTHORED IN
// TYPESCRIPT, not served by the API: GET /registry/monitor-types returns only
// {type, passive} and /registry/notification-types only {type}, so the per-field
// metadata has nowhere to come from but that file. Every `key` there is
// therefore a hand-copied duplicate of a json tag on the matching Go config
// struct, and the two drift silently — rename `webhook_url` to `url` in Go and
// nothing in either build fails. What fails is production: the create/edit
// request 400s and the form shows a generic save error, because the server
// simply ignores a field it cannot decode and then rejects the config in
// Validate().
//
// This test closes that loop. It parses registry.ts, and for each monitor type
// and notification provider asserts every declared field key is a json tag the
// corresponding Go config struct actually accepts.
//
// The check is deliberately ONE-DIRECTIONAL. A Go field with no TS field is
// fine and common: internal knobs, fields the UI intentionally does not expose,
// a backend feature landing before its UI. A TS field with no Go tag is never
// fine — the server cannot accept it. Only that direction fails.
//
// Please do not delete this as an odd test that reads the frontend from Go. It
// is the only thing tying the two halves of the config contract together.

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/monitor/types/certcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/dnscheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/dockercheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/grpccheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/httpcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/mongocheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/mqttcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/mysqlcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/ntpcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/pingcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/postgrescheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/pushcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/radiuscheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/redischeck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/snmpcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/steamcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/tcpcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/udpcheck"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/websocketcheck"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/apprise"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/discord"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/googlechat"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/gotify"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/matrix"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/mattermost"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/ntfy"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/opsgenie"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/pagerduty"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/pushover"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/rocketchat"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/signal"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/slack"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/smtp"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/teams"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/telegram"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/twilio"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/webhook"
	"github.com/Ariazonaa/statwarden/internal/notify/providers/webpush"
)

// registryTSPath is the authored field-schema file, relative to internal/api.
const registryTSPath = "../../web/src/lib/api/registry.ts"

// monitorConfigTypes maps each registry type name to the struct the checker
// unmarshals its jsonb config into. The keys are the checkers' own TypeName
// constants rather than string literals, so a renamed type breaks compilation
// here instead of quietly skipping the type.
//
// Config types are reflected over rather than parsed out of the source because
// reflect sees exactly what encoding/json sees: embedded structs flattened,
// `json:"-"` omitted, tag-less fields named after the Go field. A tag parser
// would have to re-implement those rules and would agree with the decoder only
// by luck.
var monitorConfigTypes = map[string]reflect.Type{
	certcheck.TypeName:      reflect.TypeOf(certcheck.Config{}),
	dnscheck.TypeName:       reflect.TypeOf(dnscheck.Config{}),
	dockercheck.TypeName:    reflect.TypeOf(dockercheck.Config{}),
	grpccheck.TypeName:      reflect.TypeOf(grpccheck.Config{}),
	httpcheck.TypeName:      reflect.TypeOf(httpcheck.Config{}),
	mongocheck.TypeName:     reflect.TypeOf(mongocheck.Config{}),
	mqttcheck.TypeName:      reflect.TypeOf(mqttcheck.Config{}),
	mysqlcheck.TypeName:     reflect.TypeOf(mysqlcheck.Config{}),
	ntpcheck.TypeName:       reflect.TypeOf(ntpcheck.Config{}),
	pingcheck.TypeName:      reflect.TypeOf(pingcheck.Config{}),
	postgrescheck.TypeName:  reflect.TypeOf(postgrescheck.Config{}),
	pushcheck.TypeName:      reflect.TypeOf(pushcheck.Config{}),
	radiuscheck.TypeName:    reflect.TypeOf(radiuscheck.Config{}),
	redischeck.TypeName:     reflect.TypeOf(redischeck.Config{}),
	snmpcheck.TypeName:      reflect.TypeOf(snmpcheck.Config{}),
	steamcheck.TypeName:     reflect.TypeOf(steamcheck.Config{}),
	tcpcheck.TypeName:       reflect.TypeOf(tcpcheck.Config{}),
	udpcheck.TypeName:       reflect.TypeOf(udpcheck.Config{}),
	websocketcheck.TypeName: reflect.TypeOf(websocketcheck.Config{}),
}

// notifyConfigTypes is the same table for notification providers, which carry
// their name on Type() rather than in a constant.
var notifyConfigTypes = map[string]reflect.Type{
	apprise.New().Type():    reflect.TypeOf(apprise.Config{}),
	discord.New().Type():    reflect.TypeOf(discord.Config{}),
	googlechat.New().Type(): reflect.TypeOf(googlechat.Config{}),
	gotify.New().Type():     reflect.TypeOf(gotify.Config{}),
	matrix.New().Type():     reflect.TypeOf(matrix.Config{}),
	mattermost.New().Type(): reflect.TypeOf(mattermost.Config{}),
	ntfy.New().Type():       reflect.TypeOf(ntfy.Config{}),
	opsgenie.New().Type():   reflect.TypeOf(opsgenie.Config{}),
	pagerduty.New().Type():  reflect.TypeOf(pagerduty.Config{}),
	pushover.New().Type():   reflect.TypeOf(pushover.Config{}),
	rocketchat.New().Type(): reflect.TypeOf(rocketchat.Config{}),
	signal.New().Type():     reflect.TypeOf(signal.Config{}),
	slack.New().Type():      reflect.TypeOf(slack.Config{}),
	smtp.New().Type():       reflect.TypeOf(smtp.Config{}),
	teams.New().Type():      reflect.TypeOf(teams.Config{}),
	telegram.New().Type():   reflect.TypeOf(telegram.Config{}),
	twilio.New().Type():     reflect.TypeOf(twilio.Config{}),
	webhook.New().Type():    reflect.TypeOf(webhook.Config{}),
	webpush.New().Type():    reflect.TypeOf(webpush.Config{}),
}

func TestRegistrySpecContract(t *testing.T) {
	src, err := os.ReadFile(filepath.FromSlash(registryTSPath))
	if err != nil {
		t.Fatalf("read %s: %v", registryTSPath, err)
	}
	toks := tokenizeTS(string(src))
	frags := tsFragmentKeys(toks)

	for _, tc := range []struct {
		array   string
		configs map[string]reflect.Type
		what    string
	}{
		{"monitorTypeSpecs", monitorConfigTypes, "monitor type"},
		{"notificationTypeSpecs", notifyConfigTypes, "notification provider"},
	} {
		specs := tsSpecFieldKeys(t, toks, tc.array, frags)
		if len(specs) == 0 {
			t.Fatalf("parsed no %s specs from %s — the parser stopped matching the file, "+
				"fix it rather than deleting the test", tc.array, registryTSPath)
		}
		for _, spec := range specs {
			if len(spec.keys) == 0 {
				t.Errorf("%s %q: parsed zero field keys, which is almost certainly a parser "+
					"failure rather than a genuinely field-less type", tc.what, spec.typ)
				continue
			}
			cfg, ok := tc.configs[spec.typ]
			if !ok {
				t.Errorf("%s %q is declared in %s but has no Go config struct in this test's "+
					"table, so its fields are unchecked; add it to %s",
					tc.what, spec.typ, registryTSPath, tc.array+"→config map")
				continue
			}
			tags := jsonTags(cfg)
			for _, key := range spec.keys {
				if !tags[key] {
					t.Errorf("%s %q: %s declares field key %q, but %s has no such json tag.\n"+
						"The server will drop this field and reject the config. Accepted tags: %s",
						tc.what, spec.typ, registryTSPath, key, cfg.String(),
						strings.Join(sortedKeys(tags), ", "))
				}
			}
		}
	}
}

// jsonTags returns the set of json object keys encoding/json accepts for t,
// following the same rules the decoder does.
func jsonTags(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	collectJSONTags(t, out)
	return out
}

func collectJSONTags(t reflect.Type, out map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		tag, tagged := f.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && !tagged {
			// Embedded and untagged: its fields are promoted into this object.
			collectJSONTags(f.Type, out)
			continue
		}
		if !f.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- Minimal TypeScript reader ---------------------------------------------
//
// registry.ts is data, not logic, but it is data written with object spreads
// (`hostField`, `{ ...portField(443), default: 0 }`), so a regex for `key: '…'`
// silently misses every shared fragment and would let real drift through. The
// reader below tokenizes instead: it is indifferent to formatting, comments and
// line breaks, which a line- or indent-based parser is not.

type tsToken struct {
	kind byte // 'i' identifier, 's' string literal, 'p' punctuation
	text string
}

// tokenizeTS reduces TS source to the only tokens this file cares about:
// identifiers, string literals and structural punctuation. Numbers, operators
// and comments are dropped, which is why `...spread` reads as a bare
// identifier.
func tokenizeTS(src string) []tsToken {
	var toks []tsToken
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			if n := strings.IndexByte(src[i:], '\n'); n < 0 {
				i = len(src)
			} else {
				i += n + 1
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			if n := strings.Index(src[i+2:], "*/"); n < 0 {
				i = len(src)
			} else {
				i += n + 4
			}
		case c == '\'' || c == '"' || c == '`':
			var sb strings.Builder
			i++
			for i < len(src) && src[i] != c {
				if src[i] == '\\' && i+1 < len(src) {
					i++
				}
				sb.WriteByte(src[i])
				i++
			}
			i++ // closing quote
			toks = append(toks, tsToken{'s', sb.String()})
		case isIdentByte(c) && (c < '0' || c > '9'):
			j := i
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			toks = append(toks, tsToken{'i', src[i:j]})
			i = j
		case strings.IndexByte("{}[](),:", c) >= 0:
			toks = append(toks, tsToken{'p', string(c)})
			i++
		default:
			i++
		}
	}
	return toks
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// tsFragmentKeys maps each shared FieldSpec helper to the field key it carries,
// so `hostField` and `portField(443)` can be resolved where they are spread
// into a type's field list. Helpers without a key (`tokens`) are skipped.
func tsFragmentKeys(toks []tsToken) map[string]string {
	frags := map[string]string{}
	for i := 0; i+1 < len(toks); i++ {
		if !isIdent(toks[i], "const") || toks[i+1].kind != 'i' {
			continue
		}
		if i > 0 && isIdent(toks[i-1], "export") {
			continue // the spec arrays themselves, not a field helper
		}
		name := toks[i+1].text
		for j := i + 2; j < len(toks); j++ {
			if isIdent(toks[j], "const") || isIdent(toks[j], "export") {
				break // next declaration; this helper has no key
			}
			if key, ok := keyLiteral(toks, j); ok {
				frags[name] = key
				break
			}
		}
	}
	return frags
}

type tsSpec struct {
	typ  string
	keys []string
}

// tsSpecFieldKeys walks the named spec array and returns, per `type`, every
// field key it declares — literal `key: '…'` entries plus the keys contributed
// by spread helpers. `showIf` conditions are skipped: they reference a sibling
// field's key rather than declaring one of their own.
func tsSpecFieldKeys(t *testing.T, toks []tsToken, arrayName string, frags map[string]string) []tsSpec {
	t.Helper()
	start := -1
	for i, tok := range toks {
		if isIdent(tok, arrayName) {
			for j := i; j+1 < len(toks); j++ {
				// Skip the empty brackets of the `: MonitorTypeSpec[]`
				// annotation; the first non-empty `[` opens the literal.
				if toks[j].kind == 'p' && toks[j].text == "[" && toks[j+1].text != "]" {
					start = j + 1
					break
				}
			}
			break
		}
	}
	if start < 0 {
		t.Fatalf("could not find array %s in %s", arrayName, registryTSPath)
	}

	var (
		specs []tsSpec
		cur   *tsSpec
		depth int
	)
	for i := start; i < len(toks); i++ {
		tok := toks[i]
		if tok.kind == 'p' {
			switch tok.text {
			case "{", "[":
				if depth == 0 && tok.text == "{" {
					specs = append(specs, tsSpec{})
					cur = &specs[len(specs)-1]
				}
				depth++
				continue
			case "}", "]":
				depth--
				if depth < 0 {
					return specs // end of the spec array
				}
				continue
			}
			continue
		}
		if cur == nil || tok.kind != 'i' {
			continue
		}
		switch {
		case tok.text == "type" && depth == 1:
			if v, ok := stringValue(toks, i); ok {
				cur.typ = v
			}
		case isIdent(tok, "showIf"):
			i = skipObject(toks, i)
		default:
			if key, ok := keyLiteral(toks, i); ok {
				cur.keys = append(cur.keys, key)
			} else if key, ok := frags[tok.text]; ok && !followedByColon(toks, i) {
				cur.keys = append(cur.keys, key)
			}
		}
	}
	return specs
}

// keyLiteral reports the string value of a `key: '…'` property starting at i.
func keyLiteral(toks []tsToken, i int) (string, bool) {
	if !isIdent(toks[i], "key") {
		return "", false
	}
	return stringValue(toks, i)
}

// stringValue reads the `: '…'` that follows the property name at i.
func stringValue(toks []tsToken, i int) (string, bool) {
	if i+2 >= len(toks) || toks[i+1].kind != 'p' || toks[i+1].text != ":" || toks[i+2].kind != 's' {
		return "", false
	}
	return toks[i+2].text, true
}

// followedByColon distinguishes a property name from a value reference, so the
// helper `hostField` is resolved but a property literally named `hostField`
// would not be.
func followedByColon(toks []tsToken, i int) bool {
	return i+1 < len(toks) && toks[i+1].kind == 'p' && toks[i+1].text == ":"
}

// skipObject returns the index of the closing brace of the object following i.
func skipObject(toks []tsToken, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		if toks[j].kind != 'p' {
			continue
		}
		switch toks[j].text {
		case "{":
			depth++
		case "}":
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return i
}

func isIdent(tok tsToken, name string) bool { return tok.kind == 'i' && tok.text == name }
