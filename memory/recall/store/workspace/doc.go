// Package workspace provides durable memory/recall stores backed by an
// sdk/workspace.Workspace.
//
// The backend stores recall's canonical ledger, side-effect outbox, and async
// semantic queue under one workspace subtree. It is intended for local
// development, eval runs, and single-writer deployments that want all agent
// state to share a sandboxed root without requiring SQLite or Postgres.
package workspace
