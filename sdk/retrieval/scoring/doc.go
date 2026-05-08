// Package scoring exposes the small kernel of pure functions that
// retrieval backends and fusion stages share — vector similarity and
// rank-fusion algorithms — so they can be reused without reaching
// into a backend's package-private guts or constructing a pipeline
// stage just to invoke the algorithm.
//
// All functions in this package are deterministic, allocation-aware,
// and have no dependency on workspace, embeddings, or persistence;
// they only know about retrieval.Hit and basic math.
//
// Existing call sites (sdk/retrieval/memory.cosineSim,
// sdk/retrieval/pipeline.RRFFusion) delegate to this package so
// behaviour stays identical across helpers.
package scoring
