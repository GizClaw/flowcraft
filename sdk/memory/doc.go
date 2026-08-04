// Package memory is the instance-owned runtime for typed memory
// operations.
//
// Six root operations cover the surface that FlowCraft agents need
// to persist and recall state across turns: Append (write
// transcript), Load (read transcript), Recall (retrieve long-term
// hits), Import (ingest documents and derive chunks), Compact
// (roll old transcript into summaries), and Archive (move
// compacted windows to cold destinations). Each operation is a
// strongly-typed pair of Request and Response values, addressed by
// a Scope that names a hard partition (RuntimeID + UserID) and
// any soft fields the agent cares about (AgentID,
// ConversationID, DatasetID).
//
// # Transcript records
//
// Append writes and Load reads []Record values, not
// []message.Message directly. Record wraps a Message with a
// runtime-assigned Seq and a caller-stable ID, which makes
// (Seq, ID) last-write-wins, idempotent retry (via
// AppendRequest.IdempotencyKey), and opaque cursor pagination
// (via LoadRequest.Cursor / LoadResponse.NextCursor) possible.
// Agents that only care about messages can iterate Records and
// pull .Message.
//
// # Compile authority
//
// The Compile step is the sole authority on whether a request is
// executable. Callers ask the runtime to Compile a request
// before Execute, and the runtime refuses to Execute a request
// whose canonical fields are not all accounted for in the
// decision ledger. Anything an implementation cannot honour is
// reported as a structured Decision (Native or Rejected) with a
// Reason — never a silent drop.
//
// # Integration surface
//
// memory is intentionally agnostic about how each op is wired
// into an agent. The kernel exposes 6 ops; the deploy layer
// picks which side of agent each one lives on. The mapping the
// rest of the system has converged on is:
//
//   - Append  → agent.Committer  (durable finalization, returns
//     error, agent.Identity.RunID is the idempotency key)
//   - Load    → agent.Preparer   (bounded I/O allowed)
//   - Recall  → agent.Preparer   (bounded I/O allowed)
//   - Import  → Tool factory     (called when the agent decides)
//   - Compact → scheduler        (independent goroutine, no hook)
//   - Archive → scheduler        (independent goroutine, no hook)
//
// memory is a leaf with respect to other sdk packages: it
// depends on sdk/errdefs and reuses message.Message,
// message.Part, and (via Record) inference's content union
// from sdk/inference. Concrete storage backends, embedder
// bindings, and lifecycle policies live in the adapter layer
// (sdkx/memory) and are registered through the typed Impls the
// runtime is built with. The package ships a NoopRuntime that
// satisfies every operation so deployments and tests can stand
// up an agent without a real backend.
package memory
