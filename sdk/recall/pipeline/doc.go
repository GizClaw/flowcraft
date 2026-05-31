// Package pipeline contains the recall-specific retrieval pipeline recipe and
// stages.
//
// The generic stage runner, recall lanes, fusion, rerank, post-filter and
// limit primitives live in memory/retrieval/pipeline. This package is the public
// home for long-term-memory tuning knobs and recall-owned semantics such as
// entity hints, entity-link lookup, supersedence damping and slot collapse.
package pipeline
