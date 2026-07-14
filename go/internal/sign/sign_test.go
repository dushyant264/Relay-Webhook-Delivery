package sign

import (
	"strings"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	secret, id, body := "whsec_test", "dlv_123", []byte(`{"hello":"world"}`)
	ts := time.Now()

	header := Sign(secret, id, ts, body)
	if !strings.HasPrefix(header, "v1=") {
		t.Fatalf("expected v1= prefix, got %q", header)
	}
	if err := Verify(secret, id, header, ts, body, 5*time.Minute); err != nil {
		t.Fatalf("round-trip verify failed: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	secret, id := "whsec_test", "dlv_123"
	ts := time.Now()
	header := Sign(secret, id, ts, []byte(`{"amount":100}`))

	cases := map[string]error{
		"tampered body":   Verify(secret, id, header, ts, []byte(`{"amount":999}`), 5*time.Minute),
		"wrong secret":    Verify("whsec_other", id, header, ts, []byte(`{"amount":100}`), 5*time.Minute),
		"wrong id":        Verify(secret, "dlv_999", header, ts, []byte(`{"amount":100}`), 5*time.Minute),
		"stale timestamp": Verify(secret, id, Sign(secret, id, ts.Add(-time.Hour), []byte(`{"amount":100}`)), ts.Add(-time.Hour), []byte(`{"amount":100}`), 5*time.Minute),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: expected verification to fail", name)
		}
	}
}
