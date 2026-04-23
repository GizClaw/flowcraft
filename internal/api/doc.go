// Package api implements the REST API, SSE streaming, WebSocket, and SPA hosting
// for the FlowCraft platform.
//
//go:generate go run github.com/ogen-go/ogen/cmd/ogen@v1.14.0 -clean -package oas -target oas ../../contracts/openapi.yaml
package api
