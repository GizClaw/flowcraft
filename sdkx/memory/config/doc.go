// Package config loads versioned memory deployment configuration
// and assembles *memory.Runtime instances from it.
//
// The package enforces a strict secret-free boundary: documents
// never contain credentials (embedding calls use an inference Runtime reached
// through a deploy-declared dependency), and every concrete
// store is built through an explicit StoreFactory catalog
// registered by the host binary. YAML cannot name driver code,
// so the host decides which stores exist and how they parse
// their settings; the document only says WHICH stores the
// deployment wants.
//
// # Document shape
//
// A memory.yaml looks like:
//
//	version: v1
//	runtime:
//	  hard_partition: [runtime_id, user_id]
//	  default_scope: { runtime_id: prod, user_id: tenant-1 }
//	  clock: { impl: system }
//	stores:
//	  messages:    { impl: sqlite, settings: { file: ./memory/messages.db } }
//	  documents:   { impl: sqlite, settings: { file: ./memory/documents.db } }
//	embedding:
//	  model:
//	    id: { provider: openai, name: text-embedding-3-small }
//	    profile: default
//	  dimensions: 1536
//	  batch_size: 32
//	  timeout: 30s
//	lifecycle:
//	  compact: { cron: "@hourly", older_than: 168h, keep: 50 }
//	  archive: { cron: "0 3 * * *", older_than: 2160h, destination: s3://bucket/archive }
//
// # Build pipeline
//
// The YAML deploy factory decodes the document, hands it to
// [Builder.NewAssembly] together with the inference dep already
// resolved by the deploy framework, and the Builder walks the
// stores, calls the matching StoreFactory for each, and
// assembles a *memory.Runtime plus lifecycle and typed embedding
// config used by the scheduler and document store.
package config
