package agent

import (
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// (No usage field here on purpose. The host the caller passes via
// WithEngineHost owns the engine.UsageReporter capability; if you
// need totals, accumulate inside your host implementation. Pinning
// usage here would be an end-run around the host contract and would
// silently break any host that aggregates differently — e.g. a host
// that scopes usage by tenant, by tool, or by session.)

// Status is the terminal classification of a [Run] outcome. agent does
// NOT use Status as a control-flow signal — once Run returns, the
// caller decides what to do based on Status. The values mirror the
// A2A task-status enum so they can be serialised across protocol
// boundaries without translation.
type Status string

const (
	// StatusCompleted means the engine finished cleanly and produced
	// the messages / artifacts in [Result].
	StatusCompleted Status = "completed"

	// StatusInterrupted means the engine was stopped by a cooperative
	// interrupt (host-injected). Result.Cause carries the reason.
	// By default the partial output is NOT committed (Result.Committed
	// is false); register a [Decider] (or rely on the default
	// disposition) to override.
	StatusInterrupted Status = "interrupted"

	// StatusCanceled means ctx was cancelled before the engine
	// finished.
	StatusCanceled Status = "canceled"

	// StatusFailed means the engine returned a domain error not
	// classified as interrupted / aborted.
	StatusFailed Status = "failed"

	// StatusAborted means the engine reported errdefs.IsAborted —
	// an unrecoverable internal halt. Distinguished from
	// StatusFailed so callers can apply different retry policy.
	StatusAborted Status = "aborted"
)

// Artifact is a named bundle of typed parts produced during a run
// (e.g. "summary", "tool_output_image"). Engines that write artifacts
// store them in a board channel; agent collects channel contents into
// Artifacts on the way out.
type Artifact struct {
	Name  string       `json:"name"`
	Parts []model.Part `json:"parts,omitempty"`
}

// Result is what [Run] returns after one turn. The contract:
//
//   - Run() returns (res, nil) for ALL business outcomes — completion,
//     interrupt, cancel, abort, failure. Caller inspects Status to
//     branch.
//
//   - Run() returns (nil, err) ONLY for infrastructure failures the
//     caller cannot reasonably recover from (e.g. history append
//     refused, factory returned nil engine).
//
// This mirrors sdk/workflow.Result's "W-5" rule and avoids the
// double-encoding pattern where errors are also carried by Status.
type Result struct {
	// TaskID echoes the input Request.TaskID for correlation.
	// Matches the A2A taskId casing.
	TaskID string `json:"taskId,omitempty"`

	// RunID echoes the (possibly auto-generated) execution id Run
	// used to drive the engine.
	RunID string `json:"runId,omitempty"`

	// Status classifies the outcome.
	Status Status `json:"status"`

	// Cause is set when Status == StatusInterrupted: it carries the
	// engine.Cause the host signalled. Empty otherwise.
	Cause engine.Cause `json:"cause,omitempty"`

	// Messages is the slice of NEW messages produced this turn —
	// excluding the input request and any history loaded before the
	// turn. Suitable for streaming to a UI or appending to the
	// persistent transcript (which Run already did).
	Messages []model.Message `json:"messages,omitempty"`

	// Artifacts collects named, multi-modal bundles the engine
	// emitted via dedicated board channels.
	Artifacts []Artifact `json:"artifacts,omitempty"`

	// Committed reports whether agent considered this turn's output
	// suitable for downstream commit (transcript append, archival,
	// …). It is determined by the Round B Decider chain
	// (BeforeFinalize) on top of agent's defaults:
	//
	//   - StatusCompleted defaults to Committed=true.
	//   - All non-completed statuses default to Committed=false.
	//   - Any Decider returning DiscardOutput=true forces
	//     Committed=false.
	//
	// Observers that persist transcript / artifact data are
	// expected to short-circuit when Committed is false:
	//
	//	if !res.Committed { return }
	//
	// Independent of Committed, Result.Messages always reflects the
	// engine's actual output; Committed is the *policy* signal, not
	// a content flag.
	Committed bool `json:"committed"`

	// State is a free-form bag carrying run-specific metadata. agent
	// puts a few well-known keys (run_id, board, interrupted_node,
	// …) here but does not enforce a schema beyond that.
	State map[string]any `json:"state,omitempty"`

	// Err is the engine's underlying error when Status indicates a
	// non-completed outcome. Callers that want classification call
	// errdefs.IsXxx on it; the JSON tag is "-" because errors do not
	// JSON-marshal usefully.
	Err error `json:"-"`

	// LastBoard is the engine's final Board (possibly partial when
	// Status != StatusCompleted). agent does not persist it; the
	// host can choose to checkpoint via engine's Checkpointer.
	LastBoard *engine.Board `json:"-"`
}

// Text returns the last assistant text message in Result.Messages, or
// "" if none. Convenience for chat-style callers; multi-modal callers
// should walk Messages directly.
func (r *Result) Text() string {
	if r == nil {
		return ""
	}
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != model.RoleAssistant {
			continue
		}
		t := r.Messages[i].Content()
		if t != "" {
			return t
		}
	}
	return ""
}
