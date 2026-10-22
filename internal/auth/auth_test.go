package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now()
	body := []byte(`{"libraryId":"a"}`)

	req, _ := http.NewRequest(http.MethodPost, "http://x/internal/scan", nil)
	Sign(secret, req, body, now)
	if err := Verify(secret, req, body, now); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := Verify(secret, req, []byte("tampered"), now); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := Verify(secret, req, body, now.Add(time.Hour)); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	if err := Verify([]byte("another-secret-another-secret-12"), req, body, now); err == nil {
		t.Fatal("wrong secret accepted")
	}
	other, _ := http.NewRequest(http.MethodPost, "http://x/internal/refresh", nil)
	other.Header = req.Header.Clone()
	if err := Verify(secret, other, body, now); err == nil {
		t.Fatal("signature replayed on another path")
	}
}
