package eval

import (
	"context"
	"reflect"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// phaseRunner counts the calls each phase makes, so a test can prove the
// two-phase entry points do exactly what the combined one does.
type phaseRunner struct {
	commits  int
	derives  int
	contexts int
}

func (runner *phaseRunner) CommitTurn(context.Context, corememory.Turn) error {
	runner.commits++
	return nil
}

func (runner *phaseRunner) RunOnce(context.Context) error {
	runner.derives++
	return nil
}

func (runner *phaseRunner) Context(context.Context, corememory.ContextRequest) (corememory.ContextResult, error) {
	runner.contexts++
	return corememory.ContextResult{Items: []corememory.ContextItem{{
		ID: "item", Kind: corememory.ContextRawMessage, SourceClass: corememory.ContextSourceRecent,
		Content: coremessage.NewTextContent("Alice likes tea"),
	}}}, nil
}

func phaseScenario() Scenario {
	return Scenario{
		Name: "phases", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{
			{IdempotencyKey: "t1", Messages: []coremessage.Message{
				coremessage.NewTextMessage(coremessage.RoleUser, "[D1:1] Alice: I like tea."),
			}, DatasetIDs: []string{"D1:1"}},
			{IdempotencyKey: "t2", Messages: []coremessage.Message{
				coremessage.NewTextMessage(coremessage.RoleUser, "[D1:2] Alice: Still tea."),
			}, DatasetIDs: []string{"D1:2"}},
		},
		Questions: []Question{
			{Query: "tea", WantContains: []string{"tea"}, Evidence: []string{"D1:1"}, Category: 1},
			{Query: "coffee", WantContains: []string{"coffee"}, Category: 2},
		},
	}
}

// TestPhaseSplitMatchesCombinedRun pins the contract of the split: running
// Ingest + Derive + Answer by hand produces the same report and the same call
// pattern as the combined RunWithOptions, which is what a two-phase host relies
// on.
func TestPhaseSplitMatchesCombinedRun(t *testing.T) {
	ctx := context.Background()
	combinedRunner := &phaseRunner{}
	combined, err := RunWithOptions(ctx, combinedRunner, phaseScenario(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	splitRunner := &phaseRunner{}
	if err := Ingest(ctx, splitRunner, phaseScenario()); err != nil {
		t.Fatal(err)
	}
	if err := Derive(ctx, splitRunner); err != nil {
		t.Fatal(err)
	}
	split, err := Answer(ctx, splitRunner, phaseScenario(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(normalise(combined), normalise(split)) {
		t.Fatalf("reports differ:\n combined: %#v\n split:    %#v", normalise(combined), normalise(split))
	}
	if combinedRunner.commits != splitRunner.commits || combinedRunner.derives != splitRunner.derives ||
		combinedRunner.contexts != splitRunner.contexts {
		t.Fatalf("call pattern differs: combined=%+v split=%+v", combinedRunner, splitRunner)
	}
	if combinedRunner.derives != 1 || combinedRunner.commits != 2 {
		t.Fatalf("combined run called commit=%d derive=%d", combinedRunner.commits, combinedRunner.derives)
	}
}

// TestIngestStopsBeforeDeriving documents what makes the two-phase schedule
// possible: Ingest never derives, so a host can queue several conversations
// before paying for a derivation pass.
func TestIngestStopsBeforeDeriving(t *testing.T) {
	runner := &phaseRunner{}
	if err := Ingest(context.Background(), runner, phaseScenario()); err != nil {
		t.Fatal(err)
	}
	if runner.derives != 0 || runner.contexts != 0 {
		t.Fatalf("ingest derived or answered: %+v", runner)
	}
	if err := Ingest(context.Background(), nil, phaseScenario()); err == nil {
		t.Fatal("ingest accepted a nil runner")
	}
	if err := Derive(context.Background(), nil); err == nil {
		t.Fatal("derive accepted a nil runner")
	}
}
