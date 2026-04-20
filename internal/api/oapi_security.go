package api

import (
	"context"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// HandleBearerAuth validates the JWT carried in `Authorization: Bearer ...`.
//
// Returns nil when JWT auth is disabled (`open mode`) or the token is valid.
// On failure, returns an Unauthorized error so ogenErrorHandler renders 401.
//
// NOTE: the OpenAPI spec lists `cookieAuth` and `bearerAuth` as alternative
// security schemes (OR). ogen invokes both handlers and accepts the first
// success; we therefore swallow nil-token cases here so the cookie path can
// still satisfy the request.
func (s *Server) HandleBearerAuth(ctx context.Context, _ oas.OperationName, t oas.BearerAuth) (context.Context, error) {
	if s.jwt == nil {
		return ctx, nil
	}
	if t.Token == "" {
		return ctx, errdefs.Unauthorizedf("missing bearer token")
	}
	if _, err := s.jwt.Validate(t.Token); err != nil {
		return ctx, errdefs.Unauthorizedf("invalid bearer token")
	}
	return ctx, nil
}

// HandleCookieAuth validates the JWT carried in the `flowcraft_token` cookie.
// Same semantics as HandleBearerAuth — see that comment for OR-mode behavior.
func (s *Server) HandleCookieAuth(ctx context.Context, _ oas.OperationName, t oas.CookieAuth) (context.Context, error) {
	if s.jwt == nil {
		return ctx, nil
	}
	if t.APIKey == "" {
		return ctx, errdefs.Unauthorizedf("missing session cookie")
	}
	if _, err := s.jwt.Validate(t.APIKey); err != nil {
		return ctx, errdefs.Unauthorizedf("invalid session cookie")
	}
	return ctx, nil
}
