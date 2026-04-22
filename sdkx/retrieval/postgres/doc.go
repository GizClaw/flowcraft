// Package postgres provides a retrieval.Index backed by PostgreSQL with
// pg_trgm + tsvector + jsonb (and optional pgvector for vector lanes).
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
// Hybrid is currently advertised as false (Capabilities.Hybrid=false): vector
// scoring is performed in the Pipeline, BM25-equivalent ts_rank on the server
// ( v0).
//
// Tests against a real Postgres run when env var FC_PG_DSN is set:
//
//	FC_PG_DSN=postgres://user:pass@127.0.0.1:5432/db?sslmode=disable go test ./...
package postgres
