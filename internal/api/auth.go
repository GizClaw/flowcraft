// Package api auth.go: helpers for cookie issue/clear and request authentication.
//
// The actual auth HTTP handlers (status / setup / login / logout / session /
// change-password) live in oapi_handlers.go and are wired through the
// generated OpenAPI server. Keep this file limited to plumbing that those
// handlers (and middleware) reuse.
package api

import (
	"net/http"
	"time"
)

const (
	authCookieName   = "flowcraft_token"
	settingJWTSecret = "jwt_secret"
)

// authenticateRequest validates the JWT carried by either the
// `flowcraft_token` cookie or `Authorization: Bearer ...` header.
//
// Returns (nil, true) when JWT auth is disabled (open mode), so callers can
// uniformly treat `ok == false` as "rejected".
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
