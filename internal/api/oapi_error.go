package api

import (
	"context"
	"net/http"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/errcode"
)

func newErrorResponse(err error) *oas.ErrorStatusCode {
	code, status := errcode.Resolve(err)
	return &oas.ErrorStatusCode{
		StatusCode: status,
		Response: oas.ErrorResponse{
			Error: oas.ErrorResponseError{
				Message: errcode.PublicMessage(err),
				Code:    oas.NewOptString(code),
			},
		},
	}
}

func ogenErrorHandler(_ *Server) func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	return func(ctx context.Context, w http.ResponseWriter, _ *http.Request, err error) {
		resp := newErrorResponse(err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_ = writeJSONTo(w, resp.Response)
	}
}
