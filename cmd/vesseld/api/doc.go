// Package api hosts the HTTP control plane. The default listener
// is a Unix socket (filesystem permissions are the auth boundary);
// TCP is opt-in and requires a token-file shared secret per the
// daemon document.
//
// Endpoints implemented:
//
//	POST /v1/vessels/{id}/submit          fire-and-forget Submit
//	POST /v1/vessels/{id}/call            sync wait-for-result Submit
//	GET  /v1/vessels/{id}/logs            SSE stream of stream.delta
//	GET  /v1/vessels/{id}/phase           current Captain phase
//	POST /v1/vessels/{id}/drain           graceful drain
//	POST /v1/vessels/{id}/stop            hard stop
//	GET  /v1/vessels                      list with phase
//	GET  /healthz                         "ok\n"
//	GET  /readyz                          "ready\n" once Launch finished
//	GET  /v1/plan                         redacted Plan JSON
//	GET  /v1/version                      version string
//
// All responses are JSON except SSE streams and the plain-text
// liveness probes. Errors map errdefs categories to HTTP status
// codes via writeError in errors.go.
package api
