// Package retrieval_e2e holds the black-box end-to-end tests for
// the retrieval backends shipped from sdkx, currently the
// LSM-tree-on-Workspace index in sdkx/retrieval/workspace.
//
// Why a separate Go module:
//
//   - These tests exercise the index against a REAL on-disk
//     [sdkworkspace.LocalWorkspace], not the in-memory mock. They
//     therefore poke at the filesystem directly via os.* to verify
//     atomic Rename, fsync, RemoveAll, and lockfile mtime semantics
//     that the in-package unit tests cannot observe.
//
//   - They run only when the e2e build tag is set, so the default
//     `go test ./...` from the repo root skips them.
//
//   - The module runs with GOWORK=off so its replace directives, not
//     the workspace overlay, decide which sdk/memory/sdkx sources are
//     tested. The current suite replaces those core modules to the
//     local tree so `make e2e` runs against the same source tree the
//     unit tests just exercised.
//
// Run:
//
//	cd tests/e2e/retrieval
//	go test -tags e2e -count=1 -timeout 120s ./...
package retrieval_e2e
