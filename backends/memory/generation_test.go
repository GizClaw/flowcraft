package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

const generationSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "fact": {"strategy": "none"},
  "summary": {"chunk_size": 1, "condense_threshold": 2, "group_size": 2, "max_depth": 2},
  "interval": "0"
}`

// replacingDeriver derives one fact per commit whose text carries the policy
// that produced it, so a re-derivation under a new policy is distinguishable
// from the generation it replaces.
type replacingDeriver struct{ name string }

func (deriver replacingDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	text := strings.TrimSpace(input.Content.Text())
	if text == "" {
		return nil, nil
	}
	body := deriver.name + ": " + text
	hash := factview.CanonicalHash(body)
	return []component.Artifact{{
		Kind: chat.KindFact, ID: "fact-" + strings.ReplaceAll(hash, ":", "-"),
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: body}}},
		Sources: append([]corememory.SourceRef(nil), input.Sources...),
		Metadata: corememory.Metadata{
			"canonical_hash": hash, "entities": `["Alice"]`,
			"event_time":          testNow.Format("2006-01-02T15:04:05Z07:00"),
			"source_digest":       factview.ComputeSourceDigest(input.Sources),
			"transform_signature": "replacing-derive-v1",
		},
	}}, nil
}

// TestPolicyChangeReplacesTheDerivedGeneration is the acceptance test for
// generation-scoped derivation: a policy change re-derives the same canonical
// commits into a new generation instead of accumulating beside the old one, the
// replaced generation leaves every read path in one step, and the derived views
// of a conversation carry one shared identity.
func TestPolicyChangeReplacesTheDerivedGeneration(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	// derive commits the fixture turns (idempotently: the keys are stable, so a
	// later assembly sees the same canonical commits) and runs one pass.
	derive := func(name string, version string) *Assembly {
		t.Helper()
		assembly := newTestAssemblyOn(t, ws, generationSettings,
			WithDeriver(replacingDeriver{name: name}), WithDeriverVersion(version))
		t.Cleanup(func() { _ = assembly.Close() })
		for index, text := range []string{"we talked about beverages", "and about tea"} {
			turn := corememory.Turn{
				Scope: testScope(), ConversationID: "conv-1",
				IdempotencyKey: fmt.Sprintf("run-%d", index+1),
				Messages:       []coremessage.Message{textMessage(coremessage.RoleUser, text)},
			}
			if err := assembly.CommitTurn(ctx, turn); err != nil {
				t.Fatal(err)
			}
			if err := assembly.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return assembly
	}

	first := derive("a", "policy-a")
	firstDigest := first.PolicyDigest()
	assertVisibleFacts(t, first, firstDigest, "a")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := derive("b", "policy-b")
	secondDigest := second.PolicyDigest()
	if secondDigest == firstDigest {
		t.Fatal("the new policy kept the previous digest")
	}
	visible := assertVisibleFacts(t, second, secondDigest, "b")

	// The replaced generation is still stored -- that is what makes a rollback
	// possible -- but nothing readable resolves to it.
	generations, err := second.Facts().ListGenerations(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	sorted := []string{firstDigest, secondDigest}
	sort.Strings(sorted)
	if len(generations) != 2 || generations[0] != sorted[0] || generations[1] != sorted[1] {
		t.Fatalf("generations = %#v, want %#v", generations, sorted)
	}

	// Summaries are compacted from the generation being built, never the one
	// being replaced, and the manifest carries the same identity as the facts.
	manifest, records, found, err := second.Summaries().ActiveSnapshot(ctx, testScope(), "conv-1")
	if err != nil || !found {
		t.Fatalf("active summaries = %v, %v", found, err)
	}
	if manifest.GenerationID != secondDigest {
		t.Fatalf("summary generation = %q, want %q", manifest.GenerationID, secondDigest)
	}
	live := make(map[string]struct{}, len(visible))
	for _, fact := range visible {
		live[fact.ID] = struct{}{}
	}
	if len(records) == 0 {
		t.Fatal("no active summary records")
	}
	for _, record := range records {
		// Only the leaves are derived from facts; a higher level summarises the
		// records below it, so its inputs are record ids.
		if record.Level != summaryview.L0 {
			continue
		}
		for _, inputID := range record.InputIDs {
			if _, ok := live[inputID]; !ok {
				t.Fatalf("summary %q summarises fact %q of a replaced generation", record.ID, inputID)
			}
		}
	}

	// Re-deriving under the policy that already ran converges: the visible
	// facts and the stored generations do not grow.
	if err := second.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertVisibleFacts(t, second, secondDigest, "b")
	if generations, err = second.Facts().ListGenerations(ctx, testScope(), "conv-1"); err != nil || len(generations) != 2 {
		t.Fatalf("generations after rescan = %#v, %v", generations, err)
	}

	summaryItems, err := countSummaryItems(second)
	if err != nil {
		t.Fatal(err)
	}
	if summaryItems == 0 {
		t.Fatal("the active generation's summaries are missing from the context")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	// Rolling back to the previous policy moves reads back to its generation in
	// one step: no migration, no re-derivation. Its summaries come back with
	// it, because the generation published them and the branch keeps what a
	// generation published until that generation is retired.
	rolledBack := derive("a", "policy-a")
	rolledBackFacts := assertVisibleFacts(t, rolledBack, firstDigest, "a")
	// The switch converged the projection lanes with the generation it
	// publishes, so the facts of the generation that is current again are
	// readable through them: the lane address carries the conversation, and
	// the fact it hydrates by is the store's, not the lane's.
	if factItems, err := countItems(rolledBack, corememory.ContextSourceLongTerm); err != nil {
		t.Fatal(err)
	} else if factItems == 0 {
		t.Fatal("the rolled-back generation's facts are missing from the context")
	}
	if summaryItems, err = countSummaryItems(rolledBack); err != nil {
		t.Fatal(err)
	} else if summaryItems == 0 {
		t.Fatal("the rolled-back generation's summaries are missing from the context")
	}
	// The summaries the branch serves are the ones the generation published:
	// its own leaves, summarising the facts that generation derived.
	manifest, records, found, err = rolledBack.Summaries().ActiveSnapshot(ctx, testScope(), "conv-1")
	if err != nil || !found {
		t.Fatalf("active summaries after the rollback = %v, %v", found, err)
	}
	if manifest.GenerationID != firstDigest {
		t.Fatalf("summary generation after the rollback = %q, want %q", manifest.GenerationID, firstDigest)
	}
	rolledBackLive := make(map[string]struct{}, len(rolledBackFacts))
	for _, fact := range rolledBackFacts {
		rolledBackLive[fact.ID] = struct{}{}
	}
	for _, record := range records {
		if record.Level != summaryview.L0 {
			continue
		}
		if record.GenerationID != firstDigest {
			t.Fatalf("restored summary %q belongs to generation %q", record.ID, record.GenerationID)
		}
		for _, inputID := range record.InputIDs {
			if _, ok := rolledBackLive[inputID]; !ok {
				t.Fatalf("restored summary %q summarises fact %q of another generation", record.ID, inputID)
			}
		}
	}
}

// assertVisibleFacts checks that every readable fact of the conversation belongs
// to one generation and was derived by the policy that names it.
func assertVisibleFacts(t *testing.T, assembly *Assembly, generation, policy string) []factview.Fact {
	t.Helper()
	ctx := context.Background()
	visible, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 2 {
		t.Fatalf("visible facts = %#v", visible)
	}
	for _, fact := range visible {
		if fact.Generation != generation {
			t.Fatalf("fact %q belongs to generation %q, want %q", fact.ID, fact.Generation, generation)
		}
		if !strings.HasPrefix(fact.Text, policy+": ") {
			t.Fatalf("fact %q was derived by another policy: %q", fact.ID, fact.Text)
		}
	}
	if active, found, err := assembly.Facts().ActiveGeneration(ctx, testScope(), "conv-1"); err != nil || !found || active != generation {
		t.Fatalf("active generation = %q, %v, %v", active, found, err)
	}
	return visible
}

func countSummaryItems(assembly *Assembly) (int, error) {
	return countItems(assembly, corememory.ContextSourceSummary)
}

func countItems(assembly *Assembly, class corememory.ContextSourceClass) (int, error) {
	result, err := assembly.Context(context.Background(), corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "beverages",
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range result.Items {
		if item.SourceClass == class {
			count++
		}
	}
	return count, nil
}
