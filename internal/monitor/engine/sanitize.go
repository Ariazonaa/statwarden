package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// A heartbeat's message and extra carry text the monitored target chose: an
// SNMP OctetString, a game server's name, a banner. PostgreSQL refuses a NUL
// byte or invalid UTF-8 in a text column and a \u0000 escape in jsonb, and the
// whole insert fails with it — the check then leaves no heartbeat at all.
// Cleaning once here covers every checker, including ones not written yet.

// storableText drops NUL bytes and replaces invalid UTF-8 with U+FFFD.
func storableText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.ToValidUTF8(s, "�")
}

// storableExtra returns extra unchanged when the database will accept it and a
// cleaned re-encoding otherwise. ok is false when extra is not JSON at all; the
// caller then stores none rather than losing the heartbeat with it.
func storableExtra(extra json.RawMessage) (_ json.RawMessage, ok bool) {
	if len(extra) == 0 || (utf8.Valid(extra) && !bytes.Contains(extra, []byte(`\u0000`))) {
		return extra, true
	}
	dec := json.NewDecoder(bytes.NewReader(extra))
	dec.UseNumber() // keep numbers exactly as the checker wrote them
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(cleanJSON(v))
	if err != nil {
		return nil, false
	}
	return out, true
}

// cleanJSON applies storableText to every string, keys included, of a value
// decoded from JSON.
func cleanJSON(v any) any {
	switch x := v.(type) {
	case string:
		return storableText(x)
	case []any:
		for i := range x {
			x[i] = cleanJSON(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[storableText(k)] = cleanJSON(val)
		}
		return out
	default:
		return v
	}
}
