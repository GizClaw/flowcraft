// Package memory is the first-party implementation of the core/memory
// capability contracts.
//
// The implementation covers the complete capability: canonical storage
// (conversations and documents as append-only logs with immutable revisions),
// derived views (facts, summaries, document hierarchies), projection lanes
// (BM25, entity, optional vector) fused with reciprocal-rank or calibrated
// weighted scoring and deterministic packing, the background derivation and
// summary compaction worker, and a read-only integrity verifier. Maintenance
// such as soft merging and decay is host-invoked through Assembly.Maintain,
// and the module ships no background runner.
//
// Deployments register the assembly under impl "flowcraft". The capability
// SPI exposed here matches core/memory, so deploy documents and agent hooks
// do not change when individual phases evolve.
package memory
