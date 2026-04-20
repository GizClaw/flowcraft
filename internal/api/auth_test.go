package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/sdk/errdefs"

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

func newTestServer(t *testing.T) (*Server, *oapiHandler, *stubAuthStore) {
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
	return s, newOAPIHandler(s), store
}

// withHTTP is a convenience wrapper that mimics what server.go does on the
// real request path: it stashes (w, r) in ctx so handlers can issue/clear
// cookies via HTTPResponseWriterFromContext.
func withHTTP(w http.ResponseWriter, r *http.Request) context.Context {
	return ContextWithHTTP(r.Context(), w, r)
}

// codeOf maps an errdefs error returned by an OpenAPI handler to the HTTP
// status it would produce through ogenErrorHandler. Tests previously asserted
// w.Code on raw http.Handler functions; with everything routed through ogen
// we now assert on the resolved error code instead.
func codeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return http.StatusOK
	}
	switch {
	case errdefs.IsValidation(err):
		return http.StatusBadRequest
	case errdefs.IsUnauthorized(err):
		return http.StatusUnauthorized
	case errdefs.IsConflict(err):
		return http.StatusConflict
	case errdefs.IsNotFound(err):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func TestAuthStatus_NotInitialized(t *testing.T) {
	_, h, _ := newTestServer(t)
	resp, err := h.GetAuthStatus(context.Background())
	if err != nil {
		t.Fatalf("GetAuthStatus: %v", err)
	}
	if resp.Initialized {
		t.Fatal("expected initialized=false")
	}
	if resp.AuthMode != "jwt" {
		t.Fatalf("auth_mode = %q, want jwt", resp.AuthMode)
	}
}

func TestAuthStatus_Initialized(t *testing.T) {
	_, h, store := newTestServer(t)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: "hash"}
	resp, err := h.GetAuthStatus(context.Background())
	if err != nil {
		t.Fatalf("GetAuthStatus: %v", err)
	}
	if !resp.Initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestSetup_And_DuplicateSetup(t *testing.T) {
	_, h, _ := newTestServer(t)

	resp, err := h.SetupAuth(context.Background(), &oas.SetupRequest{
		Username: "admin",
		Password: "12345678",
	})
	if err != nil {
		t.Fatalf("SetupAuth: %v", err)
	}
	if !resp.Ok {
		t.Fatal("expected ok=true")
	}

	_, err = h.SetupAuth(context.Background(), &oas.SetupRequest{
		Username: "admin",
		Password: "12345678",
	})
	if codeOf(t, err) != http.StatusConflict {
		t.Fatalf("duplicate setup: want 409, got err=%v (code=%d)", err, codeOf(t, err))
	}
}

func TestSetup_ShortPassword(t *testing.T) {
	_, h, _ := newTestServer(t)
	_, err := h.SetupAuth(context.Background(), &oas.SetupRequest{
		Username: "admin",
		Password: "short",
	})
	if codeOf(t, err) != http.StatusBadRequest {
		t.Fatalf("want 400, got err=%v", err)
	}
}

func TestSetup_EmptyUsername(t *testing.T) {
	_, h, _ := newTestServer(t)
	_, err := h.SetupAuth(context.Background(), &oas.SetupRequest{
		Username: "",
		Password: "12345678",
	})
	if codeOf(t, err) != http.StatusBadRequest {
		t.Fatalf("want 400, got err=%v", err)
	}
}

func TestLogin_Success_And_WrongPassword(t *testing.T) {
	s, h, store := newTestServer(t)

	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-password"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	resp, err := h.Login(withHTTP(w, r), &oas.LoginRequest{
		Username: "admin",
		Password: "correct-password",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected non-empty token")
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
	_ = s

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/api/auth/login", nil)
	_, err = h.Login(withHTTP(w2, r2), &oas.LoginRequest{
		Username: "admin",
		Password: "wrong",
	})
	if codeOf(t, err) != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got err=%v", err)
	}
}

func TestLogin_NotInitialized(t *testing.T) {
	_, h, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	_, err := h.Login(withHTTP(w, r), &oas.LoginRequest{
		Username: "admin",
		Password: "12345678",
	})
	if codeOf(t, err) != http.StatusUnauthorized {
		t.Fatalf("want 401, got err=%v", err)
	}
}

func TestLogout(t *testing.T) {
	_, h, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	if err := h.Logout(withHTTP(w, r)); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == authCookieName && c.MaxAge < 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected auth cookie to be cleared")
	}
}

func TestAuthSession_Authenticated(t *testing.T) {
	s, h, _ := newTestServer(t)
	token, expiresAt, _ := s.jwt.Issue("admin")
	r := httptest.NewRequest("GET", "/api/auth/session", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: token, Expires: expiresAt})

	w := httptest.NewRecorder()
	resp, err := h.GetSession(withHTTP(w, r))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !resp.Authenticated {
		t.Fatal("expected authenticated=true")
	}
	if u, ok := resp.Username.Get(); !ok || u != "admin" {
		t.Fatalf("username = %q ok=%v, want admin", u, ok)
	}
}

func TestAuthSession_Unauthenticated(t *testing.T) {
	_, h, _ := newTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/auth/session", nil)
	resp, err := h.GetSession(withHTTP(w, r))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if resp.Authenticated {
		t.Fatal("expected authenticated=false")
	}
}

func TestChangePassword(t *testing.T) {
	_, h, store := newTestServer(t)

	hash, _ := bcrypt.GenerateFromPassword([]byte("old-pass-here"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	resp, err := h.ChangePassword(context.Background(), &oas.ChangePasswordRequest{
		OldPassword: "old-pass-here",
		NewPassword: "new-pass-here",
	})
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if !resp.Ok {
		t.Fatal("expected ok=true")
	}
	if bcrypt.CompareHashAndPassword([]byte(store.cred.PasswordHash), []byte("new-pass-here")) != nil {
		t.Fatal("expected password hash to be updated")
	}
}

func TestChangePassword_WrongOldPassword(t *testing.T) {
	_, h, store := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-pw"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	_, err := h.ChangePassword(context.Background(), &oas.ChangePasswordRequest{
		OldPassword: "wrong-pw",
		NewPassword: "new-pass-here",
	})
	if codeOf(t, err) != http.StatusUnauthorized {
		t.Fatalf("want 401, got err=%v", err)
	}
}

func TestChangePassword_TooShort(t *testing.T) {
	_, h, store := newTestServer(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("old-pass-here"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	_, err := h.ChangePassword(context.Background(), &oas.ChangePasswordRequest{
		OldPassword: "old-pass-here",
		NewPassword: "short",
	})
	if codeOf(t, err) != http.StatusBadRequest {
		t.Fatalf("want 400, got err=%v", err)
	}
}

func TestChangePassword_NotInitialized(t *testing.T) {
	_, h, _ := newTestServer(t)
	_, err := h.ChangePassword(context.Background(), &oas.ChangePasswordRequest{
		OldPassword: "x",
		NewPassword: "12345678",
	})
	if codeOf(t, err) != http.StatusUnauthorized {
		t.Fatalf("want 401, got err=%v", err)
	}
}

func TestJWT_Expired_Returns401(t *testing.T) {
	s, _, store := newTestServer(t)
	s.jwt.Expiration = -1 * time.Second

	hash, _ := bcrypt.GenerateFromPassword([]byte("test"), 12)
	store.cred = &model.OwnerCredential{Username: "admin", PasswordHash: string(hash)}

	token, _, err := s.jwt.Issue("admin")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/agents", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if _, ok := s.authenticateRequest(r); ok {
		t.Fatal("expected authentication to fail for expired token")
	}
}

func TestWSTicket_RequiresJWT(t *testing.T) {
	s, _, _ := newTestServer(t)

	r := httptest.NewRequest("GET", "/api/agents", nil)
	if _, ok := s.authenticateRequest(r); ok {
		t.Fatal("expected unauthenticated request to fail")
	}

	token, _, _ := s.jwt.Issue("admin")
	r2 := httptest.NewRequest("GET", "/api/ws-ticket", nil)
	r2.Header.Set("Authorization", "Bearer "+token)
	if _, ok := s.authenticateRequest(r2); !ok {
		t.Fatal("expected authenticated request to succeed")
	}
}

func TestAuthenticateRequest_CookieAuth(t *testing.T) {
	s, _, _ := newTestServer(t)

	token, expiresAt, _ := s.jwt.Issue("cookieuser")
	r := httptest.NewRequest("GET", "/api/agents", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: token, Expires: expiresAt})

	claims, ok := s.authenticateRequest(r)
	if !ok {
		t.Fatal("expected cookie auth to succeed")
	}
	if claims.Username != "cookieuser" {
		t.Fatalf("username = %q, want cookieuser", claims.Username)
	}
}

func TestAuthenticateRequest_NilJWT(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.jwt = nil

	r := httptest.NewRequest("GET", "/api/agents", nil)
	if _, ok := s.authenticateRequest(r); !ok {
		t.Fatal("expected nil JWT to allow all requests")
	}
}
