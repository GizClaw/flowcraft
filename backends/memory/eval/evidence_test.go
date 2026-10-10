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
	// committed records the turns ingest handed to the sink, so a test can prove
	// the dataset ids reach the store instead of being dropped on the way in.
	committed []corememory.Turn
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

func (runner *evidenceRunner) CommitTurn(_ context.Context, turn corememory.Turn) error {
	runner.committed = append(runner.committed, turn)
	return nil
}

func (runner *evidenceRunner) RunOnce(context.Context) error { return nil }

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
	resolver := stubProvenance{sources: map[string][]ResolvedSource{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", Text: "[D1:1] Alice: I like tea."}},
	}}

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
	sources map[string][]ResolvedSource
}

func (resolver stubProvenance) ResolveSources(_ context.Context, item corememory.ContextItem) []ResolvedSource {
	return resolver.sources[item.ID]
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

// Ingest is the only place that knows which dataset turn a message came from,
// so it has to hand that identity to the store rather than keep it in the
// harness: grading reads it back through provenance, and a store that never
// received it can only be matched by text.
func TestIngestTagsCommittedMessagesWithDatasetTurns(t *testing.T) {
	runner := &evidenceRunner{}
	scenario := evidenceScenario()
	scenario.Turns[0].DatasetIDs = []string{"D1:1", ""}
	if _, err := RunWithOptions(context.Background(), runner, scenario, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(runner.committed) != 1 {
		t.Fatalf("committed turns = %d, want 1", len(runner.committed))
	}
	turn := runner.committed[0]
	if len(turn.MessageMetadata) != 2 {
		t.Fatalf("message metadata = %#v, want one entry per message", turn.MessageMetadata)
	}
	if got := turn.MessageMetadata[0][DatasetTurnMetadataKey]; got != "D1:1" {
		t.Fatalf("tagged turn = %q, want D1:1", got)
	}
	// A row the dataset does not name stays untagged instead of carrying an
	// empty tag that would look like a resolved turn id.
	if len(turn.MessageMetadata[1]) != 0 {
		t.Fatalf("untagged message metadata = %#v", turn.MessageMetadata[1])
	}
}

// The loader's rendering of a turn and the text the store holds for it can
// differ (an image caption the store does not carry, or the reverse), which is
// exactly the case those items are no longer lost to: the source's dataset turn
// id decides, and an item that *is* the turn's message still counts as raw text.
func TestRunMatchesEvidenceByTurnIdentityWhenTheTextDiffers(t *testing.T) {
	raw := rawItem("msg-1", "Melanie: I ran a charity race. [shared image: finish line]")
	runner := &evidenceRunner{items: []corememory.ContextItem{raw}}
	resolver := stubProvenance{sources: map[string][]ResolvedSource{
		"msg-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: "Melanie: I ran a charity race."}},
	}}
	scenario := evidenceScenario()
	scenario.Turns[0].Messages[0] = coremessage.NewTextMessage(coremessage.RoleUser, "Melanie: I ran a charity race.")

	report, err := RunWithOptions(context.Background(), runner, scenario, Options{Provenance: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if report.EvidenceFound != 1 || !report.Questions[0].Hit {
		t.Fatalf("identity did not cover the turn: %#v", report.Questions[0])
	}
	if report.Questions[0].EvidenceRaw != 1 {
		t.Fatalf("raw item classified as %#v, want a raw hit", report.Questions[0])
	}
}

// A derived item is a paraphrase, so its text never carries the turn: only the
// turn id its sources point at shows that the evidence turn was recalled.
func TestRunMatchesDerivedEvidenceByTurnIdentity(t *testing.T) {
	fact := corememory.ContextItem{
		ID: "fact-1", Kind: corememory.ContextFact, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent("Alice enjoys tea in the afternoon."),
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "conv-1/msg-1"}},
	}
	runner := &evidenceRunner{items: []corememory.ContextItem{fact}}
	resolver := stubProvenance{sources: map[string][]ResolvedSource{
		"fact-1": {{ConversationID: "conv-1", MessageID: "msg-1", TurnID: "D1:1", Text: "Alice: I like tea at four."}},
	}}

	report, err := RunWithOptions(context.Background(), runner, evidenceScenario(), Options{Provenance: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if report.EvidenceFound != 1 || report.Questions[0].EvidenceProvenance != 1 {
		t.Fatalf("derived evidence = %#v, want one provenance hit", report.Questions[0])
	}
	if report.Questions[0].EvidenceRaw != 0 {
		t.Fatalf("derived item counted as raw: %#v", report.Questions[0])
	}
}
