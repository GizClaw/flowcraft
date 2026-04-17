package api

import (
	"strings"
	"testing"
	"time"
)

func TestJWTConfig_Issue_HappyPath(t *testing.T) {
	cfg := &JWTConfig{Secret: []byte("test-secret-32-bytes-exactly-ok!")}
	token, expiresAt, err := cfg.Issue("admin")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	if expiresAt.IsZero() {
		t.Fatal("expected non-zero expiration")
	}
	if time.Until(expiresAt) < 29*24*time.Hour {
		t.Fatal("default expiration should be ~30 days")
	}
}

func TestJWTConfig_Issue_CustomExpiration(t *testing.T) {
	cfg := &JWTConfig{
		Secret:     []byte("test-secret-32-bytes-exactly-ok!"),
		Expiration: 1 * time.Hour,
	}
	_, expiresAt, err := cfg.Issue("user1")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("expected ~1h expiration, got %v", d)
	}
}

func TestJWTConfig_Validate_HappyPath(t *testing.T) {
	cfg := &JWTConfig{Secret: []byte("test-secret-32-bytes-exactly-ok!")}
	token, _, _ := cfg.Issue("testuser")

	claims, err := cfg.Validate(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Username != "testuser" {
		t.Fatalf("username = %q, want testuser", claims.Username)
	}
	if claims.Subject != "testuser" {
		t.Fatalf("subject = %q, want testuser", claims.Subject)
	}
}

func TestJWTConfig_Validate_ExpiredToken(t *testing.T) {
	cfg := &JWTConfig{
		Secret:     []byte("test-secret-32-bytes-exactly-ok!"),
		Expiration: -1 * time.Second,
	}
	token, _, _ := cfg.Issue("admin")

	_, err := cfg.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("expected 'invalid token' error, got %v", err)
	}
}

func TestJWTConfig_Validate_WrongSecret(t *testing.T) {
	issuer := &JWTConfig{Secret: []byte("secret-aaaaaaaaaaaaaaaaaaaaaaaa")}
	validator := &JWTConfig{Secret: []byte("secret-bbbbbbbbbbbbbbbbbbbbbbbb")}

	token, _, _ := issuer.Issue("admin")
	_, err := validator.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestJWTConfig_Validate_MalformedToken(t *testing.T) {
	cfg := &JWTConfig{Secret: []byte("test-secret-32-bytes-exactly-ok!")}
	_, err := cfg.Validate("not.a.jwt")
	if err == nil {
		t.Fatal("expected error for malformed token")
	}
}

func TestJWTConfig_Validate_EmptyToken(t *testing.T) {
	cfg := &JWTConfig{Secret: []byte("test-secret-32-bytes-exactly-ok!")}
	_, err := cfg.Validate("")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestGenerateSecret(t *testing.T) {
	s1, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(s1) < 32 {
		t.Fatalf("secret too short: %d", len(s1))
	}

	s2, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Fatal("two generated secrets should not be equal")
	}
}

func TestJWTConfig_DefaultExpiration(t *testing.T) {
	cfg := &JWTConfig{Secret: []byte("secret")}
	if cfg.expiration() != 30*24*time.Hour {
		t.Fatalf("expected 30 days, got %v", cfg.expiration())
	}

	cfg.Expiration = 5 * time.Minute
	if cfg.expiration() != 5*time.Minute {
		t.Fatalf("expected 5 minutes, got %v", cfg.expiration())
	}
}

func TestJWTConfig_Validate_TamperedPayload(t *testing.T) {
	cfg := &JWTConfig{Secret: []byte("test-secret-32-bytes-exactly-ok!")}
	token, _, _ := cfg.Issue("admin")

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("expected 3-part JWT")
	}
	// Tamper with payload
	parts[1] = parts[1] + "xx"
	tampered := strings.Join(parts, ".")

	_, err := cfg.Validate(tampered)
	if err == nil {
		t.Fatal("expected error for tampered token")
	}
}
