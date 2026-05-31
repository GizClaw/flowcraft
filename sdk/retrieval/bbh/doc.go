// Package bbh implements a local retrieval.Index backed by Badger, Bleve,
// and coder/hnsw.
//
// One Index instance owns the workspace root directly: badger/ stores the
// shared Badger document store, bleve/<namespace>/ stores one lazy-opened
// Bleve text index per retrieval namespace, and hnsw/<namespace>.graph stores
// periodic checkpoints of the matching HNSW graph. Badger and Bleve commit
// writes through their own embedded stores; HNSW updates the live graph and is
// checkpointed by a per-namespace flush loop plus Close. It is intended as a
// higher-performance local backend for recall's retrieval lens; the canonical
// memory truth remains the recall TemporalStore.
package bbh
