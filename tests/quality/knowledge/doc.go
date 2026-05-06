// Package knowledgequality is the in-process retrieval-quality
// regression suite for sdk/knowledge. It exercises the full
// chunk→embed→index→search pipeline against a small hand-curated
// 100-document Chinese markdown corpus so quality drifts that unit
// tests cannot see (e.g. "hybrid lane silently regresses to bm25",
// "vector lane returns the wrong cluster") get caught in CI.
//
// Why this lives outside the sdk module:
//
//   - Independent release cadence. The sdk module is the public
//     SDK; downstream consumers `go get` it routinely. Bundling
//     408KB of corpus + golden fixtures into every sdk patch tag
//     would inflate every consumer's module cache without any
//     consumer ever running these tests.
//   - Decoupled bumps. When sdk bumps, this module's `require`
//     stays pinned at the previously-tested sdk version until a
//     follow-up PR consciously revalidates against the new sdk.
//     That separation makes "is the new sdk's retrieval quality
//     OK?" a deliberate gate rather than an implicit one.
//
// Test layout:
//
//   - bm25_test.go        no external dependency; runs by default
//   - helper_test.go      service builder + corpus loader
//   - integration_test.go //go:build integration; requires
//                         EMBEDDING_PROVIDER / EMBEDDING_API_KEY /
//                         EMBEDDING_MODEL env vars to exercise the
//                         vector + hybrid lanes against a live
//                         embedding provider
//
// Invocation:
//
//	# default lane (BM25 only, no creds needed)
//	cd tests/quality/knowledge && GOWORK=off go test -count=1 ./...
//
//	# integration lane (vector + hybrid; needs .env at repo root)
//	cd tests/quality/knowledge && GOWORK=off go test -tags=integration -count=1 ./...
package knowledgequality
