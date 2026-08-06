// Package document provides the workspace-backed canonical source of
// normalized document content and provenance. Each document has an independent
// file and idempotency scope. Documents are hard-partitioned by memory scope
// and isolated by dataset. Concurrent writers in one process must share one
// WorkspaceStore.
package document
