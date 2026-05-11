package taubench

import (
	"encoding/json"
	"fmt"
	"os"
)

// UpstreamTask is the wire shape of one task in the upstream
// [τ-bench] task fixtures (data/*/tasks.json under the sierra-research
// repository). The exact field names have shifted across upstream
// revisions; we map only the fields we strictly need and tolerate
// extras via json.RawMessage on Metadata so a new column upstream
// does not break our loader.
//
// Schema this loader currently targets (verify against the upstream
// commit you're loading from — when it has drifted, add an adapter
// rather than mutating this struct):
//
//	[
//	  {
//	    "user_id":     "test_123",
//	    "instruction": "You are <persona>. Cancel my reservation that ...",
//	    "actions": [
//	      {"name": "list_user_reservations", "kwargs": {"user_id": "test_123"}},
//	      {"name": "cancel_reservation",     "kwargs": {"reservation_id": "RES-456", "reason": "..."}}
//	    ],
//	    "outputs":  ["confirmation_number=CN-..."]
//	  },
//	  ...
//	]
//
// "actions" is the GOLD trace — the canonical sequence of tool calls
// that satisfies the instruction. Our loader shadow-executes this
// trace against a clone of the initial state, captures the resulting
// State, and pins it as the task's ExpectedFinalState. The agent
// under test is scored on whether ITS final state deep-equals this
// snapshot. "outputs" entries are lifted into ExpectedTextFragments
// (case-insensitive substring matching against the agent's final
// reply).
//
// Differences from the upstream Python harness:
//
//   - We deliberately drop the Python "task.actions" feature where
//     gold actions can include conditional branches. Every task in
//     mainline τ-bench is a flat list and we fail loudly if a
//     nested action shows up so the operator knows to update the
//     loader rather than silently corrupt the gold snapshot.
//
//   - We do not implement the upstream "outputs.score = sum of llm
//     mentions" rubric; ExpectedTextFragments is a strict substring
//     match because that's what reliably ports without re-running an
//     external grader.
//
// [τ-bench]: https://arxiv.org/abs/2406.12045
type UpstreamTask struct {
	UserID      string          `json:"user_id,omitempty"`
	Instruction string          `json:"instruction"`
	Actions     []GoldAction    `json:"actions,omitempty"`
	Outputs     []string        `json:"outputs,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

// GoldAction is one entry of UpstreamTask.Actions. kwargs are
// preserved as map[string]any (not a typed struct) so we can pass
// them straight into Tool.Handler — same shape the LLM would emit.
type GoldAction struct {
	Name   string         `json:"name"`
	Kwargs map[string]any `json:"kwargs,omitempty"`
}

// LoadUpstreamTasks reads a τ-bench tasks JSON file (array of
// UpstreamTask) and converts each entry into a Task whose
// ExpectedFinalState is the snapshot produced by shadow-executing the
// gold actions on a clone of initialState. domain is stamped onto
// every resulting Task so a Report can split per-domain even when
// retail + airline are loaded together.
//
// IDs are synthesised as "<domain>-<index>" because upstream tasks
// do not carry stable ids; rerunning the loader against the same
// fixtures produces the same ids deterministically.
//
// Failures fall into three buckets:
//
//   - file IO / JSON parse: returned as-is on the error path.
//   - unknown tool name in a gold action: error mentions the action
//     index AND the tool name so the operator knows whether to
//     extend NewRetailTools / NewAirlineTools or fix the JSON.
//   - tool handler error during shadow execution: returned with
//     the offending action index so the operator can audit whether
//     the fixture's initial state is consistent with the gold trace.
func LoadUpstreamTasks(tasksPath string, initialState State, tools map[string]Tool, domain string) ([]Task, error) {
	b, err := os.ReadFile(tasksPath)
	if err != nil {
		return nil, fmt.Errorf("read tasks: %w", err)
	}
	var raw []UpstreamTask
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse tasks: %w", err)
	}

	out := make([]Task, 0, len(raw))
	for i, ut := range raw {
		taskID := fmt.Sprintf("%s-%03d", domain, i)
		expected, err := shadowRun(ut.Actions, initialState, tools)
		if err != nil {
			return nil, fmt.Errorf("task %s: shadow run: %w", taskID, err)
		}
		task := Task{
			ID:           taskID,
			Domain:       domain,
			InitialState: cloneState(initialState),
			Instruction:  ut.Instruction,
			Expected: ExpectedOutcome{
				ExpectedFinalState:    expected,
				ExpectedTextFragments: ut.Outputs,
			},
		}
		out = append(out, task)
	}
	return out, nil
}

// shadowRun executes the gold actions sequentially against a clone of
// initialState and returns the resulting state. A handler that
// returns a Go error (NOT a {"error": "..."} payload) aborts the
// shadow run — that almost always means the fixture data is
// inconsistent with the tool's preconditions and is worth surfacing
// loudly rather than silently snapshotting a half-mutated state.
//
// We tolerate handler-returned {"error": ...} payloads because some
// gold traces deliberately include a "try X, get rejected, try Y"
// pattern; the handler must still produce a deterministic state
// transition, and the snapshot captures whatever transformation
// (often: none) the rejection produced.
func shadowRun(actions []GoldAction, initialState State, tools map[string]Tool) (State, error) {
	state := cloneState(initialState)
	for i, a := range actions {
		tool, ok := tools[a.Name]
		if !ok {
			return nil, fmt.Errorf("action[%d]: unknown tool %q (extend NewRetailTools / NewAirlineTools or fix the JSON)", i, a.Name)
		}
		if _, err := tool.Handler(state, a.Kwargs); err != nil {
			return nil, fmt.Errorf("action[%d] (%s): %w", i, a.Name, err)
		}
	}
	return state, nil
}

// LoadInitialState reads a JSON file containing a single object and
// returns it as a State. Trivial; included so a CLI flag can point
// at e.g. data/retail/db.json without each call site repeating the
// boilerplate.
func LoadInitialState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return State(s), nil
}
