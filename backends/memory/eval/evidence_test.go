package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// evidenceRunner returns a fixed item set for every question.
type evidenceRunner struct {
	items []corememory.ContextItem
	// requests records the context requests so a test can prove evidence ids
	// never reach retrieval.
	requests []corememory.ContextRequest
}

// TestRunSendsOnlyTheQuestionToRetrieval pins the other half of the labelling
// boundary: retrieval sees the scope, the conversation, the question and the
// budget, and nothing else. The gold answers, the evidence ids and the category
// ride along on the Question for grading, so this test renders the request the
// runner actually issued and searches it for them -- if a later change starts
// passing a Question through (or stashing its labels in Metadata), the leak
// surfaces here rather than as an unexplained score jump.
func TestRunSendsOnlyTheQuestionToRetrieval(t *testing.T) {
	runner := &evidenceRunner{items: []corememory.ContextItem{rawItem("msg-1", "[D1:1] Alice: I like tea.")}}
	scenario := evidenceScenario()
	scenario.Questions[0].Query = "What does Alice like?"
	if _, err := RunWithOptions(context.Background(), runner, scenario, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("retrieval calls = %d, want 1", len(runner.requests))
	}
	request := runner.requests[0]
	if request.Query != "What does Alice like?" {
		t.Fatalf("query = %q", request.Query)
	}
	rendered := fmt.Sprintf("%+v", request)
	for _, leak := range []string{"tea", "D1:1", "ategory", "WantContains", "Evidence"} {
		if strings.Contains(rendered, leak) {
			t.Fatalf("dataset label %q reached retrieval: %s", leak, rendered)
		}
	}
}

func (runner *evidenceRunner) CommitTurn(context.Context, corememory.Turn) error { return nil }
func (runner *evidenceRunner) RunOnce(context.Context) error                     { return nil }

func (runner *evidenceRunner) Context(_ context.Context, request corememory.ContextRequest) (corememory.ContextResult, error) {
	runner.requests = append(runner.requests, request)
	return corememory.ContextResult{Items: runner.items}, nil
}

func evidenceScenario() Scenario {
	return Scenario{
		Name: "evidence", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Turns: []Turn{{
			IdempotencyKey: "locomo/sample-1/session_1",
			Messages: []coremessage.Message{
				coremessage.NewTextMessage(coremessage.RoleUser, "[D1:1] Alice: I like tea."),
				coremessage.NewTextMessage(coremessage.RoleAssistant, "[D1:2] Bob: Nice."),
			},
			DatasetIDs: []string{"D1:1", "D1:2"},
		}},
		Questions: []Question{
			{Query: "What does Alice like?", WantContains: []string{"tea"}, Evidence: []string{"D1:1"}, Category: 1},
		},
	}
}

func rawItem(id, text string) corememory.ContextItem {
	return corememory.ContextItem{
		ID: id, Kind: corememory.ContextRawMessage, SourceClass: corememory.ContextSourceRecent,
		Content: coremessage.NewTextContent(text),
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "conv-1/" + id}},
	}
}

func TestRunGradesEvidenceRecallFromRecalledText(t *testing.T) {
	runner := &evidenceRunner{items: []corememory.ContextItem{
		rawItem("msg-1", "[D1:1] Alice: I like tea."),
	}}
	report, err := RunWithOptions(context.Background(), runner, evidenceScenario(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.EvidenceTotal != 1 || report.EvidenceFound != 1 || report.EvidenceRecall != 1 {
		t.Fatalf("evidence = %d/%d/%v", report.EvidenceFound, report.EvidenceTotal, report.EvidenceRecall)
	}
	if !report.Questions[0].Hit || len(report.Questions[0].Missing) != 0 {
		t.Fatalf("question = %#v", report.Questions[0])
	}
	if len(runner.requests) != 1 || len(runner.requests[0].DatasetIDs) != 0 {
		t.Fatalf("evidence ids leaked into retrieval: %#v", runner.requests)
	}
}

func TestRunMissesEvidenceThatWasNotRecalled(t *testing.T) {
	runner := &evidenceRunner{items: []corememory.ContextItem{rawItem("msg-2", "[D9:9] Carol: unrelated.")}}
	report, err := RunWithOptions(context.Background(), runner, evidenceScenario(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.EvidenceFound != 0 || report.EvidenceRecall != 0 || report.Questions[0].Hit {
		t.Fatalf("report = %#v", report)
	}
	if missing := report.Questions[0].Missing; len(missing) != 1 || missing[0] != "D1:1" {
		t.Fatalf("missing = %#v", missing)
	}
}

// TestRunCountsDerivedItemsThroughProvenance pins the reason the resolver
// exists: a fact is a paraphrase, so only its provenance can show that the
// source turn was recalled.
func TestRunCountsDerivedItemsThroughProvenance(t *testing.T) {
	fact := corememory.ContextItem{
		ID: "fact-1", Kind: corememory.ContextFact, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent("Alice likes tea."),
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "conv-1/msg-1"}},
	}
	runner := &evidenceRunner{items: []corememory.ContextItem{fact}}
	resolver := stubProvenance{texts: map[string][]string{"fact-1": {"[D1:1] Alice: I like tea."}}}

	report, err := RunWithOptions(context.Background(), runner, evidenceScenario(), Options{Provenance: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if report.EvidenceFound != 1 {
		t.Fatalf("provenance was not followed: %#v", report.Questions[0])
	}
	without, err := RunWithOptions(context.Background(), runner, evidenceScenario(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if without.EvidenceFound != 0 {
		t.Fatalf("content-only matching must under-report derived items: %#v", without.Questions[0])
	}
}

type stubProvenance struct {
	texts map[string][]string
}

func (resolver stubProvenance) ResolveSourceTexts(_ context.Context, item corememory.ContextItem) []string {
	return resolver.texts[item.ID]
}

// TestRunRejectsMisalignedDatasetIDs guards the ingest-side contract: dataset
// ids must line up with messages, otherwise evidence grading would silently
// match the wrong turn.
func TestRunRejectsMisalignedDatasetIDs(t *testing.T) {
	scenario := evidenceScenario()
	scenario.Turns[0].DatasetIDs = []string{"D1:1"}
	if _, err := RunWithOptions(context.Background(), &evidenceRunner{}, scenario, Options{}); err == nil {
		t.Fatal("misaligned dataset ids were accepted")
	}
}
