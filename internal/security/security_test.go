package security

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("正确的长密码-portal-2026")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "正确的长密码-portal-2026") {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("invalid password accepted")
	}
}

func TestNormalizeMainlandPhone(t *testing.T) {
	got, err := NormalizeMainlandPhone("+86 138-0013-8000")
	if err != nil {
		t.Fatal(err)
	}
	if got != "13800138000" {
		t.Fatalf("got %q", got)
	}
	if _, err := NormalizeMainlandPhone("123"); err == nil {
		t.Fatal("invalid phone accepted")
	}
}

func TestAPIKeyShape(t *testing.T) {
	key, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) < 50 || key[:11] != "cpa_portal_" {
		t.Fatalf("unexpected key %q", key)
	}
	if len(SHA256(key)) != 64 {
		t.Fatal("unexpected hash length")
	}
}

func TestCSRF(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	token := CSRFToken(secret, "session")
	if !VerifyCSRF(secret, "session", token) {
		t.Fatal("csrf rejected")
	}
	if VerifyCSRF(secret, "other", token) {
		t.Fatal("csrf accepted for other session")
	}
}
