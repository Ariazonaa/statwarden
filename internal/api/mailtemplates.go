package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"strings"
	"time"
)

// This file is the localisation seam for transactional mail. The subscriber and
// password-reset flows render their subject and bodies here, keyed by the
// recipient's locale, so that a German subscriber gets German copy and everyone
// else falls back to English. Keeping the copy in one helper (rather than inline
// at each send site) is what lets a new locale be added in one place, and lets
// the wording be unit-tested without an SMTP server.
//
// The strings mirror the i18n catalogue entries under the `mail.*` namespace in
// web/src/lib/i18n/locales/{en,de}.ts; the two are kept in step by hand because
// the mail is rendered server-side in Go and never reaches the browser bundle.

// Supported mail locales. Anything unrecognised renders in English.
const (
	mailLocaleEN = "en"
	mailLocaleDE = "de"
)

// mailContent is a fully rendered, locale-specific message: the parts a Mailer
// needs. HTML may be empty, in which case a text-only message is sent.
type mailContent struct {
	Subject string
	Text    string
	HTML    string
}

// normalizeMailLocale reduces an arbitrary locale tag (e.g. "de", "de-DE",
// "de_AT") to one of the supported mail locales, defaulting to English. It is
// deliberately lenient: a status page's locale column is free-form, so a
// region-qualified German tag must still select the German copy.
func normalizeMailLocale(locale string) string {
	l := strings.ToLower(strings.TrimSpace(locale))
	switch {
	case l == mailLocaleDE || strings.HasPrefix(l, "de-") || strings.HasPrefix(l, "de_"):
		return mailLocaleDE
	default:
		return mailLocaleEN
	}
}

// passwordResetMail renders the password-reset mail in the recipient's locale.
// The link is built by the caller from the configured public base URL (never a
// request header), because it carries a single-use credential.
func passwordResetMail(locale, link string, ttlHours int) mailContent {
	switch normalizeMailLocale(locale) {
	case mailLocaleDE:
		return mailContent{
			Subject: "Statwarden-Passwort zurücksetzen",
			Text: fmt.Sprintf("Für dein Statwarden-Konto wurde das Zurücksetzen des Passworts angefordert.\n\n"+
				"Öffne diesen Link, um ein neues Passwort zu wählen:\n\n%s\n\n"+
				"Der Link ist %d Stunde(n) gültig und kann einmal verwendet werden. "+
				"Falls du das nicht angefordert hast, kannst du diese E-Mail ignorieren – "+
				"dein Passwort bleibt unverändert.\n", link, ttlHours),
			HTML: fmt.Sprintf(`<p>Für dein Statwarden-Konto wurde das Zurücksetzen des Passworts angefordert.</p>`+
				`<p><a href="%s">Neues Passwort wählen</a></p>`+
				`<p>Der Link ist %d Stunde(n) gültig und kann einmal verwendet werden. `+
				`Falls du das nicht angefordert hast, kannst du diese E-Mail ignorieren – `+
				`dein Passwort bleibt unverändert.</p>`, html.EscapeString(link), ttlHours),
		}
	default:
		return mailContent{
			Subject: "Reset your Statwarden password",
			Text: fmt.Sprintf("A password reset was requested for your Statwarden account.\n\n"+
				"Open this link to choose a new password:\n\n%s\n\n"+
				"The link expires in %d hour(s) and can be used once. "+
				"If you did not request this, you can ignore this mail — your password stays unchanged.\n",
				link, ttlHours),
			HTML: fmt.Sprintf(`<p>A password reset was requested for your Statwarden account.</p>`+
				`<p><a href="%s">Choose a new password</a></p>`+
				`<p>The link expires in %d hour(s) and can be used once. If you did not request this, `+
				`you can ignore this mail — your password stays unchanged.</p>`,
				html.EscapeString(link), ttlHours),
		}
	}
}

// subscribeVerifyMail renders the double-opt-in confirmation mail for a status
// page subscriber in the page's locale.
func subscribeVerifyMail(locale, pageTitle, link string) mailContent {
	title := html.EscapeString(pageTitle)
	href := html.EscapeString(link)
	switch normalizeMailLocale(locale) {
	case mailLocaleDE:
		return mailContent{
			Subject: "Abo für " + pageTitle + " bestätigen",
			Text: fmt.Sprintf("Bitte bestätige dein Abo für die Status-Updates von %s, "+
				"indem du diesen Link öffnest:\n\n%s\n", pageTitle, link),
			HTML: fmt.Sprintf(`<p>Bitte bestätige dein Abo für die Status-Updates von <strong>%s</strong>:</p>`+
				`<p><a href="%s">Abo bestätigen</a></p>`, title, href),
		}
	default:
		return mailContent{
			Subject: "Confirm your subscription to " + pageTitle,
			Text: fmt.Sprintf("Please confirm your subscription to %s status updates by visiting:\n\n%s\n",
				pageTitle, link),
			HTML: fmt.Sprintf(`<p>Please confirm your subscription to <strong>%s</strong> status updates:</p>`+
				`<p><a href="%s">Confirm subscription</a></p>`, title, href),
		}
	}
}

// Incident-notification verb tokens. The send site passes one of these so the
// subject verb can be localised; an unrecognised value falls through as-is.
const (
	IncidentVerbCreated  = "created"
	IncidentVerbUpdated  = "updated"
	IncidentVerbResolved = "resolved"
)

// incidentNotifyMail renders the incident-update mail sent to verified
// subscribers, in the page's locale. verb is one of the IncidentVerb* tokens.
func incidentNotifyMail(locale, pageTitle string, in Incident, verb, unsubLink string) mailContent {
	loc := normalizeMailLocale(locale)
	title := html.EscapeString(in.Title)
	body := html.EscapeString(in.Body)
	status := html.EscapeString(in.Status)
	severity := html.EscapeString(in.Severity)
	unsubHref := html.EscapeString(unsubLink)

	if loc == mailLocaleDE {
		return mailContent{
			Subject: fmt.Sprintf("[%s] %s — %s", pageTitle, in.Title, incidentVerbLabel(loc, verb)),
			Text: fmt.Sprintf("%s\n\nStatus: %s\nSchweregrad: %s\n\n%s\n\n---\nAbmelden: %s\n",
				in.Title, in.Status, in.Severity, in.Body, unsubLink),
			HTML: fmt.Sprintf(`<h2>%s</h2><p><strong>Status:</strong> %s<br>`+
				`<strong>Schweregrad:</strong> %s</p><p>%s</p><hr>`+
				`<p><a href="%s">Abmelden</a></p>`,
				title, status, severity, body, unsubHref),
		}
	}
	return mailContent{
		Subject: fmt.Sprintf("[%s] %s — %s", pageTitle, in.Title, incidentVerbLabel(loc, verb)),
		Text: fmt.Sprintf("%s\n\nStatus: %s\nSeverity: %s\n\n%s\n\n---\nUnsubscribe: %s\n",
			in.Title, in.Status, in.Severity, in.Body, unsubLink),
		HTML: fmt.Sprintf(`<h2>%s</h2><p><strong>Status:</strong> %s<br>`+
			`<strong>Severity:</strong> %s</p><p>%s</p><hr>`+
			`<p><a href="%s">Unsubscribe</a></p>`,
			title, status, severity, body, unsubHref),
	}
}

// incidentVerbLabel localises a verb token for the subject line. An unknown verb
// is returned verbatim so a caller passing free text still produces a subject.
func incidentVerbLabel(locale, verb string) string {
	if normalizeMailLocale(locale) == mailLocaleDE {
		switch verb {
		case IncidentVerbCreated:
			return "erstellt"
		case IncidentVerbUpdated:
			return "aktualisiert"
		case IncidentVerbResolved:
			return "behoben"
		}
		return verb
	}
	switch verb {
	case IncidentVerbCreated:
		return "created"
	case IncidentVerbUpdated:
		return "updated"
	case IncidentVerbResolved:
		return "resolved"
	}
	return verb
}

// --- RFC 5322 / MIME assembly ------------------------------------------------

// buildMIMEMessage assembles a ready-to-send message from a rendered subject and
// body pair. When html is non-empty it produces a multipart/alternative message
// (text first, then HTML, per RFC 2046 §5.1.4: least-rich part first); otherwise
// a text/plain message.
//
// Every header value is passed through sanitizeHeader so a newline smuggled into
// the subject or a recipient address cannot inject extra headers (BCC, a forged
// From) or split the message — the classic e-mail header-injection attack. The
// subject is Q-encoded so non-ASCII copy (German umlauts, em dashes) survives
// transport intact.
func buildMIMEMessage(from, to, subject, text, html string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(to) + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", sanitizeHeader(subject)) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")

	if html == "" {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
		b.WriteString("\r\n")
		b.WriteString(normalizeBody(text))
		return []byte(b.String())
	}

	boundary := mimeBoundary()
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeBody(text))
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeBody(html))
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

// mimeBoundary returns a random, RFC-safe multipart boundary. Randomness makes
// it collision-proof against a boundary marker appearing in the body.
func mimeBoundary() string {
	var buf [16]byte
	// crypto/rand.Read never returns an error on the platforms we target, but a
	// static fallback keeps the message well-formed rather than panicking.
	if _, err := rand.Read(buf[:]); err != nil {
		return "statwarden-boundary-fixed"
	}
	return "statwarden-" + hex.EncodeToString(buf[:])
}

// sanitizeHeader strips CR and LF so a value cannot inject additional headers or
// terminate the header block early. This is the load-bearing defence against
// e-mail header injection via attacker-influenced subjects or addresses.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// normalizeBody rewrites all line endings to CRLF as SMTP requires.
func normalizeBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
