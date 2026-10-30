package api

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// minPasswordLen is the minimum accepted password length. Length is the single
// most effective password rule; complexity rules are deliberately omitted.
const minPasswordLen = 8

// usernameRe is the format a login username must match: 3–32 characters of
// lowercase letters, digits, hyphen or underscore. It is intentionally narrow —
// the username is a login key served back verbatim, so it stays to an
// unambiguous, URL- and shell-safe alphabet. lower(username) is uniquely
// indexed, so 'Admin' and 'admin' would collide; requiring lowercase up front
// makes that rejection a clear 400 rather than a surprising 409.
var usernameRe = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// validateUsername enforces usernameRe and returns a client-facing 400 message.
func validateUsername(username string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("username is required")
	}
	if !usernameRe.MatchString(username) {
		return errors.New("username must be 3–32 characters of lowercase letters, digits, - or _")
	}
	return nil
}

// deriveUsername builds a fallback username from an e-mail address for callers
// that create an account without one (SSO provisioning, and setup/create
// requests that omit the field). It slugifies the local part to the username
// alphabet and falls back to "user" when nothing survives. The result is NOT
// run through validateUsername — a two-character local part is a valid derived
// name even though it is below the interactive minimum — but the store's unique
// index still guarantees it does not collide.
func deriveUsername(email string) string {
	local := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		local = email[:at]
	}
	slug := nonUsernameChars.ReplaceAllString(strings.ToLower(local), "")
	if slug == "" {
		return "user"
	}
	return slug
}

// nonUsernameChars matches every character deriveUsername strips from an e-mail
// local part: anything outside the lowercase-alnum core (dots, plus tags, …).
var nonUsernameChars = regexp.MustCompile(`[^a-z0-9]+`)

func validateEmail(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("email is required")
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Contains(email, " ") {
		return errors.New("email is not a valid address")
	}
	return nil
}

func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

// validateStatusPageSlug enforces the slug format on the TRIMMED value.
//
// Trimming first is the point, not a detail: the handlers used to check the raw
// value for emptiness while toPage trimmed afterwards, so "   " passed the check
// and was stored as an empty slug.
//
// The rule itself is domain.ValidStatusPageSlug because internal/ws enforces the
// same one on the status-page:{slug} topic and the two must not drift. Beyond
// the URL path the slug also becomes the Path attribute of the page unlock
// cookie — http.SetCookie sanitises that, so header injection was never
// reachable, but a slug containing '/' still widened or narrowed the cookie's
// scope in ways nobody asked for.
func validateStatusPageSlug(slug string) error {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return errors.New("slug is required")
	}
	if !domain.ValidStatusPageSlug(slug) {
		return errors.New("slug must be 1–64 characters of lowercase letters, digits, - or _")
	}
	return nil
}

// validateAccentColor accepts an empty value, which means "no accent colour",
// and otherwise requires a hex colour normalizeHexColor recognises. Anything
// else used to be dropped on the floor by normalizeHexColor, which answered 200
// while keeping the stored value, so the client never learned its colour was
// rejected.
func validateAccentColor(color string) error {
	if strings.TrimSpace(color) == "" {
		return nil
	}
	if normalizeHexColor(color) == "" {
		return errors.New("accent_color must be a hex colour such as #0ea5e9")
	}
	return nil
}

// validateMaintenance checks a maintenance window before it is stored. The
// rules live in the maintenance package, which is what actually evaluates the
// window, so a payload the API accepts is by construction one the engine and
// the status page can evaluate — the recurrence is parsed here rather than
// being written as opaque JSON that only fails, silently, at read time.
func validateMaintenance(m MaintenanceWindow) error {
	if strings.TrimSpace(m.Title) == "" {
		return errors.New("title is required")
	}
	w, err := m.toEvaluator()
	if err != nil {
		return err
	}
	return w.Validate()
}

// validateLinkURL guards the monitor link_url against a stored-XSS payload: it
// is rendered as an <a href> on the monitor detail page, so a "javascript:" or
// "data:" scheme sitting in the column is a script a viewing admin can trigger.
// An empty value means "no link"; anything else must be an absolute http(s)
// URL with a host. The client sanitises the same value before it renders the
// href, but the authority is here — the column must never hold anything else.
func validateLinkURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("link_url must be an absolute http:// or https:// URL")
	}
	return nil
}

func validateCredentials(email, password string) error {
	if err := validateEmail(email); err != nil {
		return err
	}
	return validatePassword(password)
}
