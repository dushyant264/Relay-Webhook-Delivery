// Package sign implements Relay's webhook signature scheme (the Stripe scheme):
//
//	Relay-Signature: v1=<hex HMAC-SHA256(secret, "{id}.{timestamp}.{body}")>
//
// Subscribers verify the signature AND reject stale timestamps (replay protection).
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const Version = "v1"

// Payload builds the exact byte string that is signed.
func Payload(id string, ts time.Time, body []byte) []byte {
	return []byte(fmt.Sprintf("%s.%d.%s", id, ts.Unix(), body))
}

// Sign returns the full header value, e.g. "v1=ab12...".
func Sign(secret, id string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(Payload(id, ts, body))
	return Version + "=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a received header value against the expected signature and
// enforces timestamp freshness.
func Verify(secret, id, header string, ts time.Time, body []byte, tolerance time.Duration) error {
	if d := time.Since(ts); d > tolerance || d < -tolerance {
		return fmt.Errorf("timestamp outside tolerance (%s old)", d)
	}
	expected := Sign(secret, id, ts, body)
	if !hmac.Equal([]byte(strings.TrimSpace(header)), []byte(expected)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
