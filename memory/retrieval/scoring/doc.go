// Package scoring exposes the small kernel of pure functions that
// retrieval backends and fusion helpers share: vector similarity and
// rank-fusion algorithms. They can be reused without reaching into a
// backend's package-private guts.
//
// All functions in this package are deterministic, allocation-aware,
// and have no dependency on workspace, embeddings, or persistence;
// they only know about retrieval.Hit and basic math.
//
// Existing call sites delegate to this package so behaviour stays
// identical across helpers.
package scoring
