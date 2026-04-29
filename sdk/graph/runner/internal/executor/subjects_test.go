package executor

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// These tests pin the graph-private subject extensions only. The
// engine-contract subjects (run.start / run.end / step.* / stream)
// are tested in sdk/engine — graph runner builds them via the engine
// builders, so re-asserting their format here would just duplicate
// the engine-side assertions.

func TestGraphPrivateSubjects_Format(t *testing.T) {
	cases := []struct {
		name string
		got  event.Subject
		want event.Subject
	}{
		{"parallel fork", subjParallelFork("r1"), "engine.run.r1.parallel.fork"},
		{"parallel join", subjParallelJoin("r1"), "engine.run.r1.parallel.join"},
		{"node skipped", subjNodeSkipped("r1", "n1"), "engine.run.r1.step.n1.skipped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, tc.got)
			}
			if err := tc.got.Validate(); err != nil {
				t.Fatalf("subject must be a valid Subject, got %v", err)
			}
		})
	}
}

func TestGraphPrivateSubjects_DotInIDDoesNotBreakSubject(t *testing.T) {
	// runID containing '.' would otherwise fragment the subject;
	// engine.SanitiseID (called by the builders) substitutes '_'.
	subj := subjNodeSkipped("run.with.dots", "node.id")
	if err := subj.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if subj != "engine.run.run_with_dots.step.node_id.skipped" {
		t.Fatalf("unexpected sanitised subject: %s", subj)
	}
}
