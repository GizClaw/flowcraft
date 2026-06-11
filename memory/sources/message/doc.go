// Package message stores canonical conversation message evidence.
//
// The package is intentionally limited to source-level persistence of raw
// messages. Higher-level behaviors such as extraction, summarization, derived
// views, and worker lifecycle orchestration are built on top of this evidence
// source by other packages.
//
// Message metadata must be JSON-compatible. Values are persisted and decoded
// with encoding/json semantics, including the usual map, slice, and number type
// normalization.
package message
