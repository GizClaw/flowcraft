package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	authCookieName = "flowcraft_token"
	settingJWTSecret = "jwt_secret"
)

func (s *Server) authenticateRequest(r *http.Request) (*Claims, bool) {
	if s.jwt == nil {
		return nil, true
	}
	if cookie, err := r.Cookie(authCookieName); err == nil && cookie.Value != "" {
		if claims, err := s.jwt.Validate(cookie.Value); err == nil {
			return claims, true
		}
	}
	if token := trimBearerToken(r.Header.Get("Authorization")); token != "" {
		if claims, err := s.jwt.Validate(token); err == nil {
			return claims, true
		}
	}
	return nil, false
}

func (s *Server) setAuthCookie(w http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
	})
}

func (s *Server) clearAuthCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	_, err := s.deps.Platform.Store.GetOwnerCredential(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": err == nil,
		"auth_mode":   "jwt",
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errdefs.Validationf("invalid request body"))
		return
	}

	cred, _ := s.deps.Platform.Store.GetOwnerCredential(r.Context())
	if cred != nil {
		writeError(w, errdefs.Conflictf("already initialized"))
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeError(w, errdefs.Validationf("username is required"))
		return
	}
	if len(req.Password) < 8 {
		writeError(w, errdefs.Validationf("password must be at least 8 characters"))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		writeError(w, errdefs.Internalf("failed to hash password"))
		return
	}

	if err := s.deps.Platform.Store.SetOwnerCredential(r.Context(), &model.OwnerCredential{
		Username:     req.Username,
		PasswordHash: string(hash),
	}); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errdefs.Validationf("invalid request body"))
		return
	}

	cred, err := s.deps.Platform.Store.GetOwnerCredential(r.Context())
	if err != nil {
		writeError(w, errdefs.Unauthorizedf("invalid credentials"))
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.Password)) != nil {
		writeError(w, errdefs.Unauthorizedf("invalid credentials"))
		return
	}

	token, expiresAt, err := s.jwt.Issue(cred.Username)
	if err != nil {
		writeError(w, errdefs.Internalf("failed to issue token"))
		return
	}

	s.setAuthCookie(w, token, expiresAt, r.TLS != nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearAuthCookie(w, r.TLS != nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authenticateRequest(r)
	if !ok || claims == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"username":      claims.Username,
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errdefs.Validationf("invalid request body"))
		return
	}

	cred, err := s.deps.Platform.Store.GetOwnerCredential(r.Context())
	if err != nil {
		writeError(w, errdefs.Unauthorizedf("not initialized"))
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.OldPassword)) != nil {
		writeError(w, errdefs.Unauthorizedf("invalid old password"))
		return
	}

	if len(req.NewPassword) < 8 {
		writeError(w, errdefs.Validationf("new password must be at least 8 characters"))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), 12)
	if err != nil {
		writeError(w, errdefs.Internalf("failed to hash password"))
		return
	}

	cred.PasswordHash = string(hash)
	if err := s.deps.Platform.Store.SetOwnerCredential(r.Context(), cred); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
