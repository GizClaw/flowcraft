// Package vesseld_e2e holds the black-box end-to-end tests for the
// vesseld binary.
//
// Why a separate Go module:
//
//   - Pinning the binary under test. The e2e helpers `go build` the
//     vesseld binary from the in-tree sources via the replace
//     directive in go.mod; running this module under GOWORK=off
//     means the build path matches what release builds will see
//     (no go.work shadowing).
//   - Dependency isolation. The e2e harness pulls in net/http
//     clients and a tiny mock OpenAI server. None of that code
//     should leak into the daemon binary or the SDK.
//
// Test layout:
//
//   - `validate_test.go` — schema-drift gate. Runs apispec +
//     resolver in validate-only mode against the multi-vessel
//     fixture under `testdata/multi-vessel/`. Fast (<100ms) and
//     therefore lives in the default `go test ./...` lane.
//   - `e2e_test.go`      — true e2e. `//go:build e2e`. Builds the
//     vesseld binary, exec's it pointing at an inline config, hits
//     the unix socket with a real http.Client, sends SIGTERM,
//     asserts a clean drain. Slower (~2s per test) so excluded
//     from default lane; invoked via `make test-e2e`.
//
// Build tag policy: the default lane stays sub-second, the
// explicit lane is allowed to spend real time exercising real OS
// resources (subprocess, sockets, signals, mock HTTP server).
package vesseld_e2e
