package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

func Sign(payload []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func Verify(payload []byte, timestamp, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}
	want := Sign(payload, timestamp, secret)
	return subtle.ConstantTimeCompare([]byte(signature), []byte(want)) == 1
}

func ValidateEventType(v string) error {
	if _, ok := SupportedEventTypes[EventType(v)]; !ok {
		return fmt.Errorf("unsupported event type %q", v)
	}
	return nil
}
