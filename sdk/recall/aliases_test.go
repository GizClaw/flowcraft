package recall_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// TestAlias_PredicateNormalizationAcrossLocales asserts that two
// extractor outputs using locale-divergent predicates ("居住地" vs
// "lives_in") collapse onto the same slot_key, so the slot supersede
// channel correctly tags the older entry as superseded.
func TestAlias_PredicateNormalizationAcrossLocales(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	var clockHolder atomic.Pointer[time.Time]
	setNow := func(t time.Time) { clockHolder.Store(&t) }
	getNow := func() time.Time { return *clockHolder.Load() }
	setNow(time.Now())
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			// Chinese locale predicate — must alias to lives_in.
			{{Content: "user lives in Guangzhou", Subject: "用户", Predicate: "居住地"}},
			// English canonical form on the second save.
			{{Content: "user lives in Shanghai", Subject: "user", Predicate: "lives_in"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithClock(getNow),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	first, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "我住在广州"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setNow(getNow().Add(time.Hour))
	second, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "I moved to Shanghai"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	doc, ok, err := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("missing original doc %q", first.EntryIDs[0])
	}
	if got := doc.Metadata["superseded_by"]; got != second.EntryIDs[0] {
		t.Fatalf("alias normalization broken: superseded_by=%v, want %q", got, second.EntryIDs[0])
	}
	if got := doc.Metadata["slot_key"]; got != "user|lives_in" {
		t.Fatalf("slot_key=%v, want canonical %q", got, "user|lives_in")
	}
}

// TestAlias_PerInstanceOverrideWins asserts that
// WithPredicateAlias-supplied entries take precedence over the
// built-in PredicateAliases table — so callers can introduce
// namespace-specific synonyms without forking the package.
func TestAlias_PerInstanceOverrideWins(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "patient sees Dr. Smith", Subject: "patient", Predicate: "primary_care"}},
			{{Content: "patient sees Dr. Johnson", Subject: "patient", Predicate: "primary_care"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithPredicateAlias(map[string]string{"primary_care": "doctor"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	first, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "1"}}},
	})
	second, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "2"}}},
	})
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if got := doc.Metadata["slot_key"]; got != "patient|doctor" {
		t.Fatalf("override missed: slot_key=%v, want %q", got, "patient|doctor")
	}
	if got := doc.Metadata["superseded_by"]; got != second.EntryIDs[0] {
		t.Fatalf("supersede did not fire after alias rewrite: superseded_by=%v", got)
	}
}

// TestAlias_CompositeSubjectPassThrough asserts that subjects
// containing ':' or '.' (used to address specific instances like
// "pet:Lucky") are NOT aliased — only the bare token is rewritten.
func TestAlias_CompositeSubjectPassThrough(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "Lucky is a labrador", Subject: "pet:Lucky", Predicate: "breed"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithSubjectAlias(map[string]string{"pet:lucky": "should_not_apply"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	res, _ := m.Save(ctx, newScope(), []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "Lucky is my dog"}}},
	})
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(newScope()), res.EntryIDs[0])
	if got := doc.Metadata["subject"]; got != "pet:Lucky" {
		t.Fatalf("composite subject was aliased: subject=%v, want %q", got, "pet:Lucky")
	}
	if got := doc.Metadata["slot_key"]; got != "pet:Lucky|breed" {
		t.Fatalf("slot_key=%v, want %q", got, "pet:Lucky|breed")
	}
}

// TestNormalizeSubject_LowercasesUnknownTokens locks in the B2 fix:
// when a subject is not in the alias table, the slot metadata MUST
// still be lowercased so two extractor outputs differing only in
// case ("Alice"/"alice") collapse onto the same slot_key. Without
// this, the second Save would write to a different slot and the
// first entry would never be superseded.
func TestNormalizeSubject_LowercasesUnknownTokens(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	var clockHolder atomic.Pointer[time.Time]
	setNow := func(t time.Time) { clockHolder.Store(&t) }
	getNow := func() time.Time { return *clockHolder.Load() }
	setNow(time.Now())
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "Alice prefers tea", Subject: "Alice", Predicate: "preference.drink"}},
			{{Content: "Alice prefers coffee", Subject: "alice", Predicate: "preference.drink"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithClock(getNow),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()
	first, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "1"}}},
	})
	setNow(getNow().Add(time.Hour))
	second, _ := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "2"}}},
	})
	doc, _, _ := idx.Get(ctx, recall.NamespaceFor(scope), first.EntryIDs[0])
	if got := doc.Metadata[recall.MetaSlotKey]; got != "alice|preference.drink" {
		t.Fatalf("subject case must be canonicalised; slot_key=%v, want %q", got, "alice|preference.drink")
	}
	if got := doc.Metadata[recall.MetaSupersededBy]; got != second.EntryIDs[0] {
		t.Fatalf("supersede must fire across case-only differences; superseded_by=%v", got)
	}
}
