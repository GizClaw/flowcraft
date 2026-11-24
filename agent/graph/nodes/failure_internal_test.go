package nodes

import (
	"testing"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/core/agent"
)

// The built-in kinds are the ones an operator reads a failing turn with, so they must report the
// diagnosis the vocabulary reserves for them. The assertions are compile-time first: a kind that
// stops implementing the capability would silently leave its failures unclassified.
var (
	_ graph.FailureClassifier = (*modelKind)(nil)
	_ graph.FailureClassifier = (*toolKind)(nil)
)

func TestBuiltInKindsClassifyTheirFailures(t *testing.T) {
	if got := (&modelKind{}).FailureKind(); got != agent.FailureModel {
		t.Fatalf("model kind reports %q, want %q", got, agent.FailureModel)
	}
	if got := (&toolKind{}).FailureKind(); got != agent.FailureTool {
		t.Fatalf("tool kind reports %q, want %q", got, agent.FailureTool)
	}
}
