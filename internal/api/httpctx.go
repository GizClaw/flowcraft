package api

import (
	"context"
	"net/http"
)

type httpCtxKey int

const (
	ctxKeyHTTPResponseWriter httpCtxKey = iota + 1
	ctxKeyHTTPRequest
)

// ContextWithHTTP attaches the active ResponseWriter and Request to ctx for ogen handlers.
func ContextWithHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	ctx = context.WithValue(ctx, ctxKeyHTTPResponseWriter, w)
	return context.WithValue(ctx, ctxKeyHTTPRequest, r)
}

// HTTPResponseWriterFromContext returns the ResponseWriter stored by ContextWithHTTP.
func HTTPResponseWriterFromContext(ctx context.Context) (http.ResponseWriter, bool) {
	w, ok := ctx.Value(ctxKeyHTTPResponseWriter).(http.ResponseWriter)
	return w, ok
}

// HTTPRequestFromContext returns the *http.Request stored by ContextWithHTTP.
func HTTPRequestFromContext(ctx context.Context) (*http.Request, bool) {
	r, ok := ctx.Value(ctxKeyHTTPRequest).(*http.Request)
	return r, ok
}
