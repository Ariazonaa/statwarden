package webhookq

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries the HMAC signature of a signed delivery:
//
//	X-Statwarden-Signature: t=1790000000,v1=5f2c…
//
// v1 is the lowercase hex HMAC-SHA256, keyed with the shared secret, over the
// string "<t>.<raw request body>". The timestamp is inside the MAC so a captured
// request cannot be replayed with a fresh time, and a receiver rejects one whose
// t is too far from its own clock (see Verify) so it cannot be replayed at all
// once that window has passed.
//
// The layout — a version-tagged value next to the timestamp — leaves room for a
// second algorithm later without breaking receivers of v1, which simply ignore
// fields they do not know.
const SignatureHeader = "X-Statwarden-Signature"

// DefaultTolerance is how far a signature's timestamp may sit from the
// receiver's clock before Verify refuses it.
const DefaultTolerance = 5 * time.Minute

// SecretProvider yields the signing secret, re-read per delivery like the URL
// so a rotation takes effect immediately. An empty secret sends the payload
// unsigned — the state of every instance nobody configured.
type SecretProvider func(ctx context.Context) (string, error)

// Sign returns the SignatureHeader value for body, signed at time at.
func Sign(secret string, at time.Time, body []byte) string {
	ts := at.Unix()
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac(secret, ts, body)))
}

// Verify checks a SignatureHeader value against body. It is what a receiver
// does — spelled out here so the documentation, the tests and any Go receiver
// share one definition — and it refuses a timestamp more than tolerance away
// from now in either direction.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts int64
	var sigs [][]byte
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return errors.New("webhookq: malformed signature timestamp")
			}
			ts = n
		case "v1":
			raw, err := hex.DecodeString(v)
			if err != nil {
				return errors.New("webhookq: malformed v1 signature")
			}
			sigs = append(sigs, raw)
		}
	}
	if ts == 0 || len(sigs) == 0 {
		return errors.New("webhookq: signature header lacks t or v1")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return errors.New("webhookq: signature timestamp outside the tolerance")
	}
	want := mac(secret, ts, body)
	for _, sig := range sigs {
		if hmac.Equal(sig, want) {
			return nil
		}
	}
	return errors.New("webhookq: signature mismatch")
}

func mac(secret string, ts int64, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(m, "%d.", ts)
	_, _ = m.Write(body)
	return m.Sum(nil)
}
