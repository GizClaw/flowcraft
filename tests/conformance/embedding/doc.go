// Package embedding hosts conformance tests that exercise sdkx
// embedding providers against their real HTTP endpoints. The suite is
// purely manual: tests `t.Skip` when EMBEDDING_PROVIDER /
// EMBEDDING_API_KEY / EMBEDDING_MODEL are not set, so a
// credential-less environment runs as a no-op. Run with:
//
//	make conformance
//
// Credentials live in a repo-root `.env` file (see
// tests/conformance/README.md).
package embedding
