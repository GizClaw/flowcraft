package mcp

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// traceTransport carries the caller's span into every request to a server.
//
// It is the whole transport policy on purpose: a retry here would replay a tool call the server may
// already have run, so reconnecting is the source's decision and not the HTTP client's, and a fixed
// timeout would cut a long tool call short — the call's own context bounds it.
type traceTransport struct{}

func (traceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	otel.GetTextMapPropagator().Inject(clone.Context(), propagation.HeaderCarrier(clone.Header))
	return http.DefaultTransport.RoundTrip(clone)
}
