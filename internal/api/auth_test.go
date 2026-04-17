package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"errors"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"golang.org/x/crypto/bcrypt"
)

type stubAuthStore struct {
	model.Store
	cred     *model.OwnerCredential
	settings map[string]string
}

var errStoreNotFound = errors.New("not found")

func (s *stubAuthStore) GetOwnerCredential(_ context.Context) (*model.OwnerCredential, error) {
	if s.cred == nil {
		return nil, errStoreNotFound
	}
	return s.cred, nil
}

func (s *stubAuthStore) SetOwnerCredential(_ context.Context, c *model.OwnerCredential) error {
	s.cred = c
	return nil
}

func (s *stubAuthStore) GetSetting(_ context.Context, key string) (string, error) {
	v, ok := s.settings[key]
	if !ok {
		return "", errStoreNotFound
	}
	return v, nil
}

func (s *stubAuthStore) SetSetting(_ context.Context, key, value string) error {
	if s.settings == nil {
		s.settings = make(map[string]string)
	}
	s.settings[key] = value
	return nil
}

func newTestServer(t *testing.T) (*Server, *stubAuthStore) {
	t.Helper()
	store := &stubAuthStore{settings: make(map[string]string)}
	jwtCfg := &JWTConfig{Secret: []byte("test-secret-32-bytes-exactly-ok!")}
	deps := ServerDeps{
		Platform: &platform.Platform{Store: store},
	}
	s := &Server{
		deps:      deps,
		jwt:       jwtCfg,
		wsTickets: newWSTicketStore(),
		done:      make(chan struct{}),
	}
	return s, store
}

func TestAuthStatus_NotInitialized(t *testing.T) {
	s, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/auth/status", nil)
	s.handleAuthStatus(w, r)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["initialized"] != false {
		t.Fatal("expected initialized=false")
	}
}

func TestSetup_And_DuplicateSetup(t *testing.T) {
	s, _ := newTestServer(t)

	w := httptest.NewRecorder()
	body := `{"username":"admin","password":"12345678"}`
	r := httptest.NewRequest("POST", "/api/auth/setup", strings.NewReader(body))
	s.handleSetup(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/api/auth/setup", strings.NewReader(body))
	s.handleSetup(w2, r2)
	if w2.Code != 409 {
		t.Fatalf("expected 409 for duplicate setup, got %d", w2.Code)
	}
}

func TestLogin_Success_And_WrongPassword(t *testing.T) {
	s, store := newTestServer(t)

	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-password"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"username":"admin","password":"correct-password"}`))
	s.handleLogin(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["token"] == nil || body["token"] == "" {
		t.Fatal("expected token in response")
	}

	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == authCookieName {
			found = true
		}
	}
	if !found {
		t.Fatal("expected auth cookie to be set")
	}

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"username":"admin","password":"wrong"}`))
	s.handleLogin(w2, r2)
	if w2.Code != 401 {
		t.Fatalf("expected 401 for wrong password, got %d", w2.Code)
	}
}

func TestJWT_Expired_Returns401(t *testing.T) {
	s, store := newTestServer(t)
	s.jwt.Expiration = -1 * time.Second

	hash, _ := bcrypt.GenerateFromPassword([]byte("test"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	token, _, err := s.jwt.Issue("admin")
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/agents", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	_, ok := s.authenticateRequest(r)
	if ok {
		t.Fatal("expected authentication to fail for expired token")
	}
	_ = w
}

func TestChangePassword(t *testing.T) {
	s, store := newTestServer(t)

	hash, _ := bcrypt.GenerateFromPassword([]byte("old-pass-here"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/change-password",
		strings.NewReader(`{"old_password":"old-pass-here","new_password":"new-pass-here"}`))
	s.handleChangePassword(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if bcrypt.CompareHashAndPassword([]byte(store.cred.PasswordHash), []byte("new-pass-here")) != nil {
		t.Fatal("expected password hash to be updated")
	}
}

func TestWSTicket_RequiresJWT(t *testing.T) {
	s, _ := newTestServer(t)

	r := httptest.NewRequest("GET", "/api/agents", nil)
	_, ok := s.authenticateRequest(r)
	if ok {
		t.Fatal("expected unauthenticated request to fail")
	}

	token, _, _ := s.jwt.Issue("admin")
	r2 := httptest.NewRequest("GET", "/api/ws-ticket", nil)
	r2.Header.Set("Authorization", "Bearer "+token)
	_, ok2 := s.authenticateRequest(r2)
	if !ok2 {
		t.Fatal("expected authenticated request to succeed")
	}
}
