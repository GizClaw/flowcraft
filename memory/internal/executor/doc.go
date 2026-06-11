// Package executor runs one configured memory capability flow at a time.
//
// The executor is deliberately below the public control plane. It wires
// compiler assemblies to stores, projection writers, and capability services,
// then performs vertical operations such as chunk indexing, summary building,
// observation extraction, fact reconciliation, graph building, projection
// search, and raw context packing. It does not own public plans, scheduling, or
// lifecycle orchestration.
package executor
