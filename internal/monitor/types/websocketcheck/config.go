package websocketcheck

// Secrets lists the JSON field names in Config that carry credentials. The API
// redacts them from read responses and keeps a submitted redaction placeholder
// as "unchanged"; the SecretsBox seals the whole config at rest. Handshake
// headers are included wholesale because they routinely carry cookies and API
// keys.
func (*Checker) Secrets() []string {
	return []string{"headers"}
}
