package agenttest

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
)

// Result runs the result suite against one ResultReader. build must return a
// fresh engine for each subtest; the suite fails when the engine does not
// implement the capability it claims to test.
func Result(t *testing.T, build func(t *testing.T) agent.Runner) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, func(*testing.T) agent.Runner)
	}{
		{"StateMatchesTheRunner", resultStateMatches},
		{"CompletedCarriesTheAnswer", resultCarriesTheAnswer},
		{"InputRequiredCarriesTheQuestion", resultCarriesTheQuestion},
		{"ResultIsReadOnly", resultIsReadOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, build) })
	}
}

func resultReader(t *testing.T, build func(*testing.T) agent.Runner) (agent.Runner, agent.ResultReader) {
	t.Helper()
	runner := build(t)
	reader, ok := runner.(agent.ResultReader)
	if !ok {
		t.Fatalf("%T does not implement agent.ResultReader", runner)
	}
	return runner, reader
}

func resultStateMatches(t *testing.T, build func(*testing.T) agent.Runner) {
	runner, reader := resultReader(t, build)
	if _, err := drain(runner.Run(context.Background(), turn())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	result, err := reader.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.State != runner.State() {
		t.Fatalf("result state = %s, runner state = %s", result.State, runner.State())
	}
}

func resultCarriesTheAnswer(t *testing.T, build func(*testing.T) agent.Runner) {
	runner, reader := resultReader(t, build)
	events, err := drain(runner.Run(context.Background(), turn()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.State() != agent.StateCompleted {
		t.Skipf("runner ended in %s, not completed", runner.State())
	}
	var text strings.Builder
	for _, event := range events {
		if typed, ok := event.(agent.Text); ok {
			text.WriteString(typed.Text)
		}
	}
	if text.Len() == 0 {
		t.Skip("the runner reported no text")
	}
	result, err := reader.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := result.Content.Text(); got != text.String() {
		t.Fatalf("result content = %q, want %q", got, text.String())
	}
}

func resultCarriesTheQuestion(t *testing.T, build func(*testing.T) agent.Runner) {
	runner, reader := resultReader(t, build)
	if _, err := drain(runner.Run(context.Background(), turn())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.State() != agent.StateInputRequired {
		t.Skipf("runner ended in %s, not input-required", runner.State())
	}
	result, err := reader.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Question == nil || result.Question.Text() == "" {
		t.Fatalf("input-required result has no question: %+v", result)
	}
}

func resultIsReadOnly(t *testing.T, build func(*testing.T) agent.Runner) {
	runner, reader := resultReader(t, build)
	if _, err := drain(runner.Run(context.Background(), turn())); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first, err := reader.Result(context.Background())
	if err != nil {
		t.Fatalf("first Result: %v", err)
	}
	second, err := reader.Result(context.Background())
	if err != nil {
		t.Fatalf("second Result: %v", err)
	}
	if first.State != second.State || first.Content.Text() != second.Content.Text() {
		t.Fatalf("Result changed between reads: %+v vs %+v", first, second)
	}
}
