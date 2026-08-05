// Package message provides the workspace-backed canonical source of raw
// conversation messages. Each idempotent turn is an immutable commit file.
// Records are hard-partitioned by memory scope and isolated by conversation.
// Concurrent writers in one process must share one WorkspaceStore.
package message
