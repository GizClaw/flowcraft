// Package llm hosts conformance tests that exercise sdkx LLM providers
// against their real HTTP endpoints. The suite is purely manual:
// individual tests `t.Skip` when the corresponding FLOWCRAFT_TEST_*
// env var is not set, so a credential-less environment runs as a
// no-op. Run with:
//
//	make conformance
//
// or directly:
//
//	cd tools/conformance && go test -count=1 ./llm/...
//
// Credentials live in a repo-root `.env` file (see
// tools/conformance/README.md).
package llm
