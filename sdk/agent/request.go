package agent

import (
	"slices"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// Request is one agent turn submitted to [Run].
//
// Field names and JSON tags mirror the A2A protocol's MessageSendParams
// schema (camelCase: taskId, contextId, …) so requests can be
// serialised across the protocol without translation. The notable
// absence vs sdk/workflow.Request is that Request does not carry a
// RuntimeID (Run is now a stateless function) and does not carry a
// Strategy hint (the engine is supplied directly to Run).
type Request struct {
	// TaskID identifies a long-lived task the request is part of.
	// Empty when the caller is not tracking tasks. Maps to A2A's
	// "taskId".
	TaskID string `json:"taskId,omitempty"`

	// ContextID identifies the conversation / session. Used as the
	// conversation key passed to History.Load / Append. Empty means
	// "no persistent transcript for this turn". Maps to A2A's
	// "contextId".
	ContextID string `json:"contextId,omitempty"`

	// RunID is the host-supplied execution id. When empty Run mints
	// one. The same value is propagated as Run.RunID and as the
	// run id attribute on emitted events. Not part of the A2A wire
	// schema — it is an internal correlation key, kept camelCase for
	// stylistic consistency.
	RunID string `json:"runId,omitempty"`

	// Message is the user's turn input (text, parts, attachments).
	Message inference.Message `json:"message"`

	// Inputs are arbitrary structured inputs the engine reads off
	// the Board (form fields, parameters, …). They are written under
	// their map keys as Board vars before the engine starts.
	Inputs map[string]any `json:"inputs,omitempty"`

	// Config carries per-request preferences (output modes, …).
	// Maps to A2A's "configuration".
	Config *RequestConfig `json:"configuration,omitempty"`
}

// RequestConfig holds per-request preferences. Optional knobs are
// added here rather than on Request to keep Request stable across
// minor versions. JSON keys mirror the A2A MessageSendConfiguration
// schema so requests can flow across the protocol without
// translation.
type RequestConfig struct {
	// AcceptedOutputModes constrains what modalities the caller can
	// receive (e.g. ["text/plain"], ["audio/wav"], …). Engines that
	// can produce multiple modalities consult it to pick one. Maps
	// to A2A's "acceptedOutputModes".
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
}

// ---------- Result ----------

// (No usage field here on purpose. The host the caller passes via
// WithHost owns the UsageReporter capability; if you
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
	// is false); register a [Referee] (or rely on the default
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
	Name  string           `json:"name"`
	Parts []inference.Part `json:"parts,omitempty"`
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
	// used to drive the
	RunID string `json:"runId,omitempty"`

	// Status classifies the outcome.
	Status Status `json:"status"`

	// Cause is set when Status == StatusInterrupted: it carries the
	// Cause the host signalled. Empty otherwise.
	Cause Cause `json:"cause,omitempty"`

	// Messages is the slice of NEW messages produced this turn —
	// excluding the input request and any history loaded before the
	// turn. Suitable for streaming to a UI or appending to the
	// persistent transcript (which Run already did).
	Messages []inference.Message `json:"messages,omitempty"`

	// Artifacts collects named, multi-modal bundles the engine
	// emitted via dedicated board channels.
	Artifacts []Artifact `json:"artifacts,omitempty"`

	// Committed reports whether this turn's output was accepted and,
	// when Committers are registered, durably committed. Agent first
	// derives commit eligibility from the Referee chain on top of its
	// defaults:
	//
	//   - StatusCompleted defaults to Committed=true.
	//   - All non-completed statuses default to Committed=false.
	//   - Any Referee returning AcceptOutput=true makes the result
	//     eligible for the normal Committer chain.
	//   - Any Referee returning DiscardOutput=true forces
	//     Committed=false, overriding AcceptOutput.
	//   - A Committer failure forces Committed=false and Execute
	//     returns the Result together with that error.
	//
	// Independent of Committed, Result.Messages always reflects the
	// engine's actual output. When no Committer is registered,
	// Committed reflects acceptance alone.
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
	LastBoard *Board `json:"-"`

	// Attempts is the number of Execute invocations Run made
	// before settling on this Result. 1 for fresh (single-shot)
	// runs; >1 only when WithMaxRevise was enabled and at least
	// one Referee returned Decision{Revise: true}.
	//
	// Attempts is the post-loop count, not "remaining budget":
	// Attempts == 2 means the engine was invoked twice. Hooks
	// reading res.Attempts in OnRunEnd see the final value.
	//
	// Zero is reserved for "Run never reached Execute"
	// (infrastructure error). Real runs always have Attempts >= 1.
	Attempts int `json:"attempts,omitempty"`
}

// Text returns the last assistant text message in Result.Messages, or
// "" if none. Convenience for chat-style callers; multi-modal callers
// should walk Messages directly.
func (r *Result) Text() string {
	if r == nil {
		return ""
	}
	for _, v := range slices.Backward(r.Messages) {
		if v.Role != inference.RoleAssistant {
			continue
		}
		t := v.Content.Text()
		if t != "" {
			return t
		}
	}
	return ""
}
