// Package postgres provides a retrieval.Index backed by PostgreSQL with
// tsvector + jsonb metadata filtering helpers.
//
// One namespace == one regular table named retrieval_<ns>:
//
//	id          TEXT PRIMARY KEY
//	content     TEXT NOT NULL
//	tsv         tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED
//	metadata    jsonb
//	vector      bytea
//	sparse      jsonb
//	ts          timestamptz NOT NULL
//
// Capabilities.Vector and Capabilities.Hybrid are false: this adapter does not
// support native vector scoring or multi-signal hybrid search. QueryText uses
// ts_rank over tsvector; SparseVec uses in-process sparse dot-product scoring
// over the stored sparse jsonb payload.
//
// Tests against a real Postgres run when env var FC_PG_DSN is set:
//
//	FC_PG_DSN=postgres://user:pass@127.0.0.1:5432/db?sslmode=disable go test ./...
//
// Performance note: List currently scans the whole namespace into memory
// before applying filter and pagination. It is sized for management /
// console use; large-scale exports should use Iterate, which streams docs
// in id order with bounded memory.
package postgres
