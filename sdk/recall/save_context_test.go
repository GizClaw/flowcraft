package recall_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

func TestSaveUsesExistingFactsContext(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{{
			{Content: "User still prefers oat milk latte", Categories: []string{"preferences"}},
		}},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithSaveContext(3, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	if _, err := m.Add(ctx, scope, recall.Entry{
		Content:    "User prefers oat milk latte at the office cafe",
		Categories: []string{"preferences"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Please remember I still order oat milk latte every morning."}}},
	}); err != nil {
		t.Fatal(err)
	}

	if ex.calls != 1 {
		t.Fatalf("extractor called %d times, want 1", ex.calls)
	}
	if len(ex.gotExisting) != 1 || len(ex.gotExisting[0]) == 0 {
		t.Fatalf("expected existing facts context, got %+v", ex.gotExisting)
	}
	if !strings.Contains(ex.gotExisting[0][0], "oat milk latte") {
		t.Fatalf("existing facts did not include recalled snippet: %+v", ex.gotExisting[0])
	}
}

func TestSaveWithContextIgnoresRecallFailure(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{{
			{Content: "User wants terse commit messages", Categories: []string{"preferences"}},
		}},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithSaveContext(3, 0),
		recall.WithPipeline(pipeline.New(failingStage{name: "boom", err: errors.New("recall failed")})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	res, err := m.Save(ctx, newScope(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Keep commit messages short."}}},
	})
	if err != nil {
		t.Fatalf("Save should ignore recall-context failure, got %v", err)
	}
	if len(res.EntryIDs) != 1 {
		t.Fatalf("entry_ids=%v", res.EntryIDs)
	}
	if len(ex.gotExisting) != 1 || len(ex.gotExisting[0]) != 0 {
		t.Fatalf("expected extractor to see no existing facts after recall failure, got %+v", ex.gotExisting)
	}
	if _, ok, err := idx.Get(ctx, recall.NamespaceFor(newScope()), res.EntryIDs[0]); err != nil || !ok {
		t.Fatalf("saved entry missing after recall-context failure: ok=%v err=%v", ok, err)
	}
}
