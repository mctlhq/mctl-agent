package webhook

import "testing"

func TestSignVerify(t *testing.T) {
	payload := []byte(`{"hello":"world"}`)
	ts := "2026-03-25T10:00:00Z"
	secret := "super-secret"
	sig := Sign(payload, ts, secret)
	if !Verify(payload, ts, sig, secret) {
		t.Fatal("expected signature to verify")
	}
	if Verify(payload, ts, sig, "wrong-secret") {
		t.Fatal("signature should not verify with wrong secret")
	}
}
