// Package agent is one turn of an agent: the vocabulary a caller streams, the contract
// a driver implements, and the decorator that runs a turn under one identity.
//
// # What it is not
//
// It is not the session table. Sessions, their eviction, their limits and the one-run-
// per-session rule belong to `host`, which is the only layer that holds the process.
// It is not the loop either: a driver decides what the steps of a turn are. This package
// never calls a model and never dispatches a tool.
//
// And it is not the noun "Agent" in the domain model: that is a *deployment* — model,
// tools, prompt, driver — which `deploy` produces and `runtime` holds. The package is
// named for the turn, not for the deployment, and that distinction is the reason it has
// no configuration surface of its own.
//
// # The shapes
//
// [Event] is a closed set. A consumer can switch over every kind, and the compiler keeps
// that switch honest when a kind is added. A non-streaming caller folds the sequence; a
// streaming one forwards it, and nothing in between may buffer it.
//
// [Task] and its [Checkpoint] are here too: a task is what survives a process, a turn is
// what happens inside one, and a turn that could not be resumed would not need either.
//
// # The contract
//
// [Driver] runs a turn: it takes a [Request], yields events, and closes. It does not own
// state — where the task stands is in the checkpoint, what the session may do is in the
// binding — so a driver can be rebuilt after a restart and resumed from the store.
//
// Two capabilities are optional and separate from Driver:
//
//   - [Steerer]: apply operator input at a step boundary, and let it be withdrawn until
//     it is applied.
//   - [Canceller]: forward a cancellation to a peer, for a driver that talks to one.
//
// # The turn decorator
//
// [Turn] is what a host calls: it attaches turn identity, opens the span, forwards the
// driver's events unchanged, saves the terminal checkpoint and classifies the failure.
// Putting that in one place is what keeps every driver from re-deriving it — and what
// makes the observability rules in docs/plans/agent-turn.md hold for drivers written
// later.
package agent
