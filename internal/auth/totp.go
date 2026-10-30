package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 is the RFC 6238 / authenticator-app standard for TOTP
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters fixed to the values every authenticator app defaults to
// (Google Authenticator, Aegis, 1Password …): SHA-1, 6 digits, 30-second step.
const (
	totpDigits    = 6
	totpPeriod    = 30 * time.Second
	totpSecretLen = 20 // 160-bit secret, the RFC-recommended size
	// totpSkew allows the adjacent time steps so a code entered near a
	// boundary (or with minor clock drift) still verifies.
	totpSkew = 1
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh base32-encoded TOTP secret suitable for
// storing in users.totp_secret and embedding in an otpauth:// URI.
func NewTOTPSecret() (string, error) {
	b := make([]byte, totpSecretLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read totp secret: %w", err)
	}
	return base32NoPad.EncodeToString(b), nil
}

// TOTPURI builds the otpauth:// provisioning URI encoded into the enrolment QR
// code. issuer and account are shown in the authenticator app.
func TOTPURI(secret, issuer, account string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", totpDigits))
	q.Set("period", fmt.Sprintf("%d", int(totpPeriod.Seconds())))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// TOTPCode computes the code for secret at time t. Exposed for tests, which
// need to produce a valid code; production code verifies with VerifyTOTP
// instead of recomputing.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("auth: decode totp secret: %w", err)
	}
	counter := uint64(t.Unix()) / uint64(totpPeriod.Seconds()) //nolint:gosec // G115: a clock before 1970 breaks more than TOTP
	return hotp(key, counter), nil
}

// VerifyTOTP reports whether code is valid for secret at time now, tolerating
// one step of clock skew on either side. Comparison is constant-time.
func VerifyTOTP(secret, code string, now time.Time) bool {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return false
	}
	// An empty key is refused HERE, not left to the callers.
	//
	// base32 decodes "" successfully, so an empty or whitespace-only secret used to
	// produce a valid HMAC key of zero length — and the codes for a zero-length key
	// are a fixed function of the time step, which anyone can compute. Whoever holds
	// that verifies as the second factor.
	//
	// No caller reaches it today: totp_enabled is only ever written together with a
	// secret, and openTOTPSecret hands back an unopenable envelope verbatim rather
	// than "" precisely so it fails the decode above. But that is four separate
	// arguments holding one property up, one of them about the SHAPE of a
	// ciphertext, and users.totp_enabled has no constraint tying it to a non-empty
	// users.totp_secret — so the unsafe state is representable in SQL even though no
	// code path writes it. A primitive whose contract is "never call me with an
	// empty secret" should enforce that itself; the comment at auth_handlers.go:171
	// says a caller-side check "is not redundant", which is the same observation
	// from the other end.
	if len(key) == 0 {
		return false
	}
	code = strings.TrimSpace(code)
	step := uint64(totpPeriod.Seconds())
	counter := uint64(now.Unix()) / step //nolint:gosec // G115: as in TOTPCode
	for delta := -totpSkew; delta <= totpSkew; delta++ {
		c := counter
		if delta < 0 {
			c -= uint64(-delta)
		} else {
			c += uint64(delta)
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, c)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// hotp is the RFC 4226 HMAC-based one-time password used as TOTP's core.
func hotp(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	return fmt.Sprintf("%0*d", totpDigits, value%1_000_000)
}
