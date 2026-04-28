package executor

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/event"
)

func TestSubjects_Format(t *testing.T) {
	cases := []struct {
		name string
		got  event.Subject
		want event.Subject
	}{
		{"graph start", subjGraphStart("r1"), "graph.run.r1.start"},
		{"graph end", subjGraphEnd("r1"), "graph.run.r1.end"},
		{"parallel fork", subjParallelFork("r1"), "graph.run.r1.parallel.fork"},
		{"parallel join", subjParallelJoin("r1"), "graph.run.r1.parallel.join"},
		{"node start", subjNodeStart("r1", "n1"), "graph.run.r1.node.n1.start"},
		{"node complete", subjNodeComplete("r1", "n1"), "graph.run.r1.node.n1.complete"},
		{"node error", subjNodeError("r1", "n1"), "graph.run.r1.node.n1.error"},
		{"node skipped", subjNodeSkipped("r1", "n1"), "graph.run.r1.node.n1.skipped"},
		{"stream delta", subjNodeStreamDelta("r1", "n1"), "graph.run.r1.node.n1.stream.delta"},
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

func TestSubjects_PatternsValidate(t *testing.T) {
	patterns := []event.Pattern{
		PatternRun("r1"),
		PatternAllRuns(),
		PatternRunNodes("r1"),
	}
	for _, p := range patterns {
		if err := p.Validate(); err != nil {
			t.Fatalf("pattern %q invalid: %v", p, err)
		}
	}

	// Cross-check Matches against a couple of subjects.
	if !PatternRun("r1").Matches("graph.run.r1.start") {
		t.Fatal("PatternRun should match start")
	}
	if !PatternRun("r1").Matches("graph.run.r1.node.n1.complete") {
		t.Fatal("PatternRun should match node events")
	}
	if PatternRun("r1").Matches("graph.run.r2.start") {
		t.Fatal("PatternRun must not cross runs")
	}
	if !PatternRunNodes("r1").Matches("graph.run.r1.node.n1.start") {
		t.Fatal("PatternRunNodes should match")
	}
	if PatternRunNodes("r1").Matches("graph.run.r1.start") {
		t.Fatal("PatternRunNodes must not match graph-level events")
	}
}

func TestSanitiseID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "_"},
		{"r1", "r1"},
		{"a.b", "a_b"},
		{"a*b", "a_b"},
		{"a>b", "a_b"},
		{"a.b*c>d", "a_b_c_d"},
		{"normal-id_123", "normal-id_123"},
	}
	for _, tc := range cases {
		if got := sanitiseID(tc.in); got != tc.want {
			t.Errorf("sanitiseID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSubjects_DotInIDDoesNotBreakSubject(t *testing.T) {
	// runID containing '.' would otherwise fragment the subject.
	subj := subjNodeStart("run.with.dots", "node.id")
	if err := subj.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if subj != "graph.run.run_with_dots.node.node_id.start" {
		t.Fatalf("unexpected sanitised subject: %s", subj)
	}
}
