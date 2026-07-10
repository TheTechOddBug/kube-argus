package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// ConfigAcceptsEvent treats an empty Events list as "subscribe to everything".
func TestConfigAcceptsEvent_EmptyMeansAll(t *testing.T) {
	cfg := Config{Events: nil}
	if !ConfigAcceptsEvent(cfg, "jit.approved") {
		t.Fatal("empty Events should accept any event")
	}
	if !ConfigAcceptsEvent(cfg, "jit.denied") {
		t.Fatal("empty Events should accept any event")
	}
}

func TestConfigAcceptsEvent_FiltersByEventName(t *testing.T) {
	cfg := Config{Events: []string{"jit.approved", "jit.denied"}}

	if !ConfigAcceptsEvent(cfg, "jit.approved") {
		t.Fatal("subscribed event should be accepted")
	}
	if ConfigAcceptsEvent(cfg, "jit.requested") {
		t.Fatal("unsubscribed event should be rejected")
	}
}

// Sign must produce a stable HMAC-SHA256 that consumers can verify.
func TestWebhookSign_MatchesStandardHmac(t *testing.T) {
	body := []byte(`{"event":"jit.approved"}`)
	secret := "test-shared-secret"

	got := Sign(body, secret)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("signature mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestWebhookSign_DifferentBodyDifferentSig(t *testing.T) {
	s1 := Sign([]byte("a"), "secret")
	s2 := Sign([]byte("b"), "secret")
	if s1 == s2 {
		t.Fatal("different bodies must produce different signatures")
	}
}
