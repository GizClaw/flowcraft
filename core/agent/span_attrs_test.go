package agent_test

import (
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// TestTurnSpanAttributesAreAllKnown is the leak half of the observability rules.
//
// internal/layout forbids the identifiers on a *metric label*; a span is where they
// belong, which makes a span the one place a body, a prompt or a credential can end up in
// a backend nobody audited. "Who added this key" is a review question only until the test
// answers it: every attribute this flow emits is named here, with the reason it is safe to
// ship to a trace store.
//
// The keys arrive from two places, which is why this is a dynamic test and not a scan of
// the call sites: the span builder sets a handful, and telemetry.AttrsFor injects the
// identity of the turn. Neither is visible to the other, and a scan of either alone would
// miss half of what a trace actually contains.
func TestTurnSpanAttributesAreAllKnown(t *testing.T) {
	// known maps an attribute key to why it may leave the process. Anything not here is
	// either a mistake or a decision that has not been made yet.
	known := map[string]string{
		"agent.name":        "the agent's name, the join key every series and panel uses",
		"agent.round":       "the turn's ordinal within its task",
		"agent.state":       "the terminal state vocabulary",
		"agent.trace_level": "the level this turn ran at, so a sparse trace explains itself",
		"session.id":        "which conversation, which is what a trace is for",
		"task.id":           "which task, so a resumed run joins its predecessor",
		"tenant.id":         "the deployment's tenant, the one identifier a backend groups by",
		// The identifier a metric label may not carry lives here instead: a span is one
		// row somebody is already reading, and "which turn" is the question it answers.
		"turn":            "the turn's ordinal, which internal/layout keeps off every metric label",
		"session.bind.id": "the digest of the narrowed grant — a digest, not the grant",
	}

	recorder := telemetrytest.New(t)
	driver := agenttest.NewScripted(agent.Text{Text: "x"}, agenttest.Completed()).OpenDriver()
	if _, err := collect(agent.RunTurn(sessionCtx(t), request(nil), driver)); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	seen := map[string]bool{}
	for _, span := range recorder.Spans() {
		for key := range telemetrytest.Attrs(span) {
			seen[key] = true
			if _, ok := known[key]; !ok {
				t.Errorf("span %q carries attribute %q, which is not in the allowlist; "+
					"either it is a leak or it is a decision to record in the test", span.Name(), key)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no span attributes were observed; the turn emitted nothing to check")
	}
	// An allowlist that covers keys nothing emits is a list that will not be read again.
	keys := make([]string, 0, len(known))
	for key := range known {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			t.Logf("allowlisted attribute %q was not emitted by this flow", key)
		}
	}
}
