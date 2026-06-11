// Package projectors converts semantic view records into indexed records.
//
// Projectors are internal, deterministic adapters between memory/views records
// and memory/internal/views/indexed.Record. They also own physical projection
// namespace layout helpers. They do not define view stores, hold retrieval
// writers or indexes, or write to a retrieval backend.
package projectors
