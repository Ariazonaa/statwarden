package api

import (
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMailLocale(t *testing.T) {
	cases := map[string]string{
		"de":       mailLocaleDE,
		"de-DE":    mailLocaleDE,
		"de_AT":    mailLocaleDE,
		"DE":       mailLocaleDE,
		"  de-CH ": mailLocaleDE,
		"en":       mailLocaleEN,
		"en-GB":    mailLocaleEN,
		"fr":       mailLocaleEN, // unsupported → English fallback
		"":         mailLocaleEN,
		"garbage":  mailLocaleEN,
	}
	for in, want := range cases {
		if got := normalizeMailLocale(in); got != want {
			t.Errorf("normalizeMailLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPasswordResetMailLocalised pins that the reset mail renders in the
// recipient's language: German for a German locale, English otherwise, with the
// credential link preserved verbatim in both.
func TestPasswordResetMailLocalised(t *testing.T) {
	const link = "https://statwarden.example/reset-password?token=abc123"

	de := passwordResetMail("de-DE", link, 1)
	if !strings.Contains(de.Subject, "Passwort") {
		t.Errorf("German subject = %q, want it to mention Passwort", de.Subject)
	}
	if !strings.Contains(de.Text, "Passwort") || !strings.Contains(de.Text, link) {
		t.Errorf("German text missing localisation or link: %q", de.Text)
	}
	if !strings.Contains(de.HTML, link) {
		t.Errorf("German HTML missing link: %q", de.HTML)
	}

	en := passwordResetMail("en", link, 1)
	if !strings.Contains(en.Subject, "Reset") {
		t.Errorf("English subject = %q, want it to mention Reset", en.Subject)
	}
	if !strings.Contains(en.Text, "password reset") && !strings.Contains(en.Text, "password") {
		t.Errorf("English text not English: %q", en.Text)
	}
	if en.Subject == de.Subject {
		t.Errorf("English and German subjects are identical (%q); localisation did nothing", en.Subject)
	}
}

func TestSubscribeVerifyMailLocalised(t *testing.T) {
	const (
		title = "Acme Status"
		link  = "https://statwarden.example/status/acme/verify?token=tok"
	)
	de := subscribeVerifyMail("de", title, link)
	if !strings.Contains(de.Subject, title) || !strings.Contains(de.Subject, "bestätigen") {
		t.Errorf("German subject = %q, want it to name the page and use German", de.Subject)
	}
	if !strings.Contains(de.Text, link) {
		t.Errorf("German text missing link: %q", de.Text)
	}

	en := subscribeVerifyMail("en", title, link)
	if !strings.Contains(en.Subject, "Confirm") {
		t.Errorf("English subject = %q, want Confirm", en.Subject)
	}
	if en.Text == de.Text {
		t.Error("English and German verification bodies are identical; localisation did nothing")
	}
}

func TestIncidentNotifyMailLocalised(t *testing.T) {
	in := Incident{Title: "API errors", Body: "We are investigating.", Status: "investigating", Severity: "major"}
	const unsub = "https://statwarden.example/status/acme/unsubscribe?token=u"

	de := incidentNotifyMail("de", "Acme Status", in, IncidentVerbResolved, unsub)
	if !strings.Contains(de.Subject, "behoben") {
		t.Errorf("German subject = %q, want the verb localised to behoben", de.Subject)
	}
	if !strings.Contains(de.Text, "Schweregrad") || !strings.Contains(de.Text, "Abmelden") {
		t.Errorf("German body not localised: %q", de.Text)
	}

	en := incidentNotifyMail("en", "Acme Status", in, IncidentVerbResolved, unsub)
	if !strings.Contains(en.Subject, "resolved") {
		t.Errorf("English subject = %q, want resolved", en.Subject)
	}
	if !strings.Contains(en.Text, "Severity") || !strings.Contains(en.Text, "Unsubscribe") {
		t.Errorf("English body not localised: %q", en.Text)
	}
}

// TestIncidentVerbLabelUnknownPassesThrough pins that a free-text verb the send
// site did not tokenise still yields a usable subject rather than an empty one.
func TestIncidentVerbLabelUnknownPassesThrough(t *testing.T) {
	if got := incidentVerbLabel("en", "Incident updated"); got != "Incident updated" {
		t.Errorf("incidentVerbLabel(en, free text) = %q, want it returned verbatim", got)
	}
	if got := incidentVerbLabel("de", "Incident updated"); got != "Incident updated" {
		t.Errorf("incidentVerbLabel(de, free text) = %q, want it returned verbatim", got)
	}
}

// TestIncidentNotifyMailEscapesHTML pins that attacker-influenced incident text
// cannot inject markup into the HTML part of the mail.
func TestIncidentNotifyMailEscapesHTML(t *testing.T) {
	in := Incident{Title: "<script>alert(1)</script>", Body: "a & b < c", Status: "investigating", Severity: "minor"}
	m := incidentNotifyMail("en", "Acme", in, IncidentVerbCreated, "https://x/u")
	if strings.Contains(m.HTML, "<script>") {
		t.Errorf("HTML part contains an unescaped <script> tag: %q", m.HTML)
	}
	if !strings.Contains(m.HTML, "&amp;") {
		t.Errorf("HTML part did not escape the ampersand in the body: %q", m.HTML)
	}
}

// --- MIME assembly ------------------------------------------------------------

func TestBuildMIMEMessageMultipart(t *testing.T) {
	raw := buildMIMEMessage("from@example.com", "to@example.com", "Hello", "plain body", "<p>html body</p>", fixedNow())
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("ParseMediaType: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediaType)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		types = append(types, p.Header.Get("Content-Type"))
	}
	if len(types) != 2 {
		t.Fatalf("got %d parts, want 2: %v", len(types), types)
	}
	// RFC 2046 §5.1.4: least-rich (text) part first, HTML last.
	if !strings.HasPrefix(types[0], "text/plain") || !strings.HasPrefix(types[1], "text/html") {
		t.Errorf("part order = %v, want text/plain then text/html", types)
	}
}

func TestBuildMIMEMessageTextOnly(t *testing.T) {
	raw := buildMIMEMessage("from@example.com", "to@example.com", "Hi", "just text", "", fixedNow())
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if ct := msg.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// TestBuildMIMEMessageQEncodesSubject pins that non-ASCII subjects (German
// umlauts) survive transport: the raw header must be an RFC 2047 encoded-word,
// and a decoder must recover the original text.
func TestBuildMIMEMessageQEncodesSubject(t *testing.T) {
	const subject = "Statwarden-Passwort zurücksetzen"
	raw := buildMIMEMessage("from@example.com", "to@example.com", subject, "body", "", fixedNow())
	if strings.Contains(string(raw), subject) {
		t.Errorf("raw message contains the un-encoded non-ASCII subject; it must be Q-encoded")
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("DecodeHeader: %v", err)
	}
	if decoded != subject {
		t.Errorf("decoded subject = %q, want %q", decoded, subject)
	}
}

// TestBuildMIMEMessageRejectsHeaderInjection is the security test for the
// header-injection defence. A subject or recipient carrying CRLF must NOT be
// able to inject a new header (a hidden Bcc) or split the message. Without
// sanitizeHeader stripping CR/LF this test fails: the parser would see a real
// Bcc header. Verified by reverting sanitizeHeader to the identity function and
// observing this test fail.
func TestBuildMIMEMessageRejectsHeaderInjection(t *testing.T) {
	t.Run("via subject", func(t *testing.T) {
		raw := buildMIMEMessage(
			"from@example.com", "to@example.com",
			"Hello\r\nBcc: victim@evil.example", // injection attempt
			"body", "", fixedNow())
		msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if bcc := msg.Header.Get("Bcc"); bcc != "" {
			t.Errorf("header injection via subject succeeded: Bcc = %q", bcc)
		}
	})

	t.Run("via recipient", func(t *testing.T) {
		raw := buildMIMEMessage(
			"from@example.com",
			"to@example.com\r\nBcc: victim@evil.example", // injection attempt
			"Hello", "body", "", fixedNow())
		msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if bcc := msg.Header.Get("Bcc"); bcc != "" {
			t.Errorf("header injection via recipient succeeded: Bcc = %q", bcc)
		}
		if strings.Contains(msg.Header.Get("To"), "\n") {
			t.Errorf("To header still contains a newline: %q", msg.Header.Get("To"))
		}
	})
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
}
