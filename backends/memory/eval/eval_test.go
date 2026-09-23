package eval

import (
	"context"
	"strings"
	"testing"

	flowcraftmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/component"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

type stubRunner struct {
	answer string
}

// TestRunSkipsUngradedQuestionsAndKeepsCategories pins the grading contract:
// blank or absent expectations make a question ungraded (excluded from the
// hit rate), and dataset categories survive into the report.
func TestRunSkipsUngradedQuestionsAndKeepsCategories(t *testing.T) {
	scenario := Scenario{
		Name: "categories", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{{IdempotencyKey: "t1", Messages: []coremessage.Message{{
			Role:    coremessage.RoleUser,
			Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "hello"}}},
		}}}},
		Questions: []Question{
			{Query: "tea", WantContains: []string{"Alice likes tea"}, Category: 1},
			{Query: "coffee", WantContains: []string{"coffee"}, Category: 2},
			{Query: "empty", WantContains: []string{"  "}},
			{Query: "absent"},
		},
	}
	report, err := Run(context.Background(), stubRunner{answer: "Alice likes tea"}, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if report.Ungraded != 2 || report.Hits != 1 || report.HitRate != 0.5 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Questions) != 4 || !report.Questions[2].Ungraded || report.Questions[2].Hit {
		t.Fatalf("questions = %#v", report.Questions)
	}
	if report.ByCategory[1] != (CategoryStats{Questions: 1, Hits: 1}) {
		t.Fatalf("category 1 = %#v", report.ByCategory)
	}
	if report.ByCategory[2] != (CategoryStats{Questions: 1, Hits: 0}) {
		t.Fatalf("category 2 = %#v", report.ByCategory)
	}
}

func (runner stubRunner) CommitTurn(context.Context, corememory.Turn) error { return nil }
func (runner stubRunner) RunOnce(context.Context) error                     { return nil }

func (runner stubRunner) Context(context.Context, corememory.ContextRequest) (corememory.ContextResult, error) {
	return corememory.ContextResult{Items: []corememory.ContextItem{{
		ID: "item-1", Kind: corememory.ContextFact, Score: 1,
		Content:     coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: runner.answer}}},
		Sources:     []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "m1"}},
		SourceClass: corememory.ContextSourceLongTerm,
	}}}, nil
}

func TestRunGradesQuestions(t *testing.T) {
	scenario := Scenario{
		Name: "smoke", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{{IdempotencyKey: "t1", Messages: []coremessage.Message{{
			Role:    coremessage.RoleUser,
			Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "hello"}}},
		}}}},
		Questions: []Question{
			{Query: "tea", WantContains: []string{"Alice likes tea"}},
		},
	}
	report, err := Run(context.Background(), stubRunner{answer: "Alice likes tea"}, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if report.Hits != 1 || report.HitRate != 1 {
		t.Fatalf("report = %#v", report)
	}
	miss, err := Run(context.Background(), stubRunner{answer: "unrelated"}, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if miss.Hits != 0 || len(miss.Questions[0].Missing) != 1 {
		t.Fatalf("miss report = %#v", miss)
	}
}

// fakeDeriver mirrors the chat contract with a deterministic fact so the
// harness can run against the real assembly.
type fakeDeriver struct{}

func (fakeDeriver) Derive(context.Context, component.Artifact) ([]component.Artifact, error) {
	sources := []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "conv-1/msg-1", Revision: "1"}}
	return []component.Artifact{{
		Kind: "fact", ID: "fact-alice-tea",
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "Alice likes tea"}}},
		Sources: sources,
		Metadata: corememory.Metadata{
			"canonical_hash":      factview.CanonicalHash("Alice likes tea"),
			"event_time":          "2026-09-18T12:00:00Z",
			"source_digest":       factview.ComputeSourceDigest(sources),
			"transform_signature": "eval-v1",
		},
	}}, nil
}

func TestRunAgainstAssembly(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "interval": "0"
	}`
	value, err := flowcraftmemory.NewFactory(flowcraftmemory.WithDeriver(fakeDeriver{})).New(
		context.Background(), resource.Input{
			Settings: []byte(settings),
			Deps:     map[string]any{"workspace": ws},
		})
	if err != nil {
		t.Fatal(err)
	}
	assembly, ok := value.(Runner)
	if !ok {
		t.Fatalf("assembly does not satisfy Runner: %T", value)
	}
	scenario := Scenario{
		Name: "assembly-smoke", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{{IdempotencyKey: "t1", Messages: []coremessage.Message{{
			Role:    coremessage.RoleUser,
			Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "we talked about beverages"}}},
		}}}},
		Questions: []Question{{Query: "tea", WantContains: []string{"Alice likes tea"}}},
	}
	report, err := Run(context.Background(), assembly, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if report.HitRate != 1 {
		t.Fatalf("assembly report = %#v", report)
	}
	if _, err := Run(context.Background(), assembly, Scenario{}); err == nil {
		t.Fatal("invalid scenario accepted")
	}
}

func TestLoadScenariosJSONL(t *testing.T) {
	input := `# locomo export
{"name":"one","scope":{"runtime_id":"memories"},"conversation_id":"conv-1","questions":[]}

{"name":"two","scope":{"runtime_id":"memories"},"conversation_id":"conv-2","questions":[]}
`
	scenarios, err := LoadScenariosJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 2 || scenarios[0].Name != "one" || scenarios[1].Name != "two" {
		t.Fatalf("scenarios = %#v", scenarios)
	}
	if _, err := LoadScenariosJSONL(strings.NewReader(`{"name":"bad","unknown":true}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
