// Package e2e exercises the knowledge stack end-to-end against a small
// hand-curated 100-document Chinese markdown corpus. The goal is to
// catch retrieval-quality regressions that unit tests can't see (e.g.
// "hybrid lane silently regresses to bm25", "vector lane returns the
// wrong cluster"), without depending on any external service.
//
// Layout:
//
//	testdata/corpus/*.md   — 100 markdown docs across 10 clusters
//	testdata/golden.jsonl  — 30 (question, expected_doc, expected_keywords)
//
// Modes:
//
//	BM25   — runs by default, no external dependency.
//	Vector — //go:build integration, requires real Embedder envs.
//	Hybrid — //go:build integration, ditto.
//
// Cross-mode invariants ("hybrid >= bm25") live in the integration
// file because they need both lanes to be live.
package e2e_test
