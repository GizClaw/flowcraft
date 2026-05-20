package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// TestDefaultStructurizer_KindFallbackIsNote pins the post-route-2
// contract: when neither the caller nor the LLM extractor populated
// f.Kind, the Structurizer's fallback is a stable KindNote. The
// earlier keyword table was deleted (Route-2 diagnostics show it
// fires on 0% of LLM-extracted facts) — this test guards against
// the next refactor accidentally inferring something more aggressive
// that would behave unpredictably for callers using the slim schema.
func TestDefaultStructurizer_KindFallbackIsNote(t *testing.T) {
	cases := []string{
		"Alice loves black coffee in the morning.",
		"Alice plans to visit Paris next month.",
		"Alice is married to Bob.",
		"Alice lives in San Francisco.",
		"Alice went to the cinema yesterday.",
		"Some unmarked observation.",
	}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			f := domain.TemporalFact{Content: content}
			out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
			if out.Kind != domain.KindNote {
				t.Errorf("kind = %q, want KindNote (fallback path)", out.Kind)
			}
		})
	}
}

// TestDefaultStructurizer_HonoursCustomEntityExtractor verifies that
// the EntityExtractor field swaps in cleanly so callers can plug in
// LLM- or NER-service-based extractors without forking
// DefaultStructurizer. This is the architectural payoff of
// promoting the (99.9% load-bearing) NER sub-task to its own
// interface.
func TestDefaultStructurizer_HonoursCustomEntityExtractor(t *testing.T) {
	custom := stubEntityExtractor{out: []string{"plugged-in"}}
	s := DefaultStructurizer{EntityExtractor: custom}
	f := domain.TemporalFact{Content: "Alice met Bob at Paris."}
	out := s.Structurize(f, port.IngestInput{})
	if len(out.Entities) != 1 || out.Entities[0] != "plugged-in" {
		t.Errorf("custom extractor not honoured, got %v", out.Entities)
	}
}

type stubEntityExtractor struct{ out []string }

func (s stubEntityExtractor) ExtractEntities(string, []port.EntitySnapshot) []string {
	return append([]string(nil), s.out...)
}

// TestDiffStructurizerCoverage_OnlyCountsEmpty→NonEmpty pins the
// load-bearing invariant that the Structurizer's coverage counters
// MUST only count the fields the stage actually flipped from empty
// to non-empty. Pre-populated facts (passthrough extractor) and
// untouched fields must not inflate the counters, otherwise the
// resulting ratios overstate how much work each sub-task does.
func TestDiffStructurizerCoverage_OnlyCountsEmptyToNonEmpty(t *testing.T) {
	cases := []struct {
		name   string
		before domain.TemporalFact
		after  domain.TemporalFact
		want   diagnostic.StructurizerCoverage
	}{
		{
			name:   "all_fields_filled_by_stage",
			before: domain.TemporalFact{},
			after: domain.TemporalFact{
				Kind: domain.KindState, Subject: "alice", Entities: []string{"alice"},
				Metadata: map[string]any{MetaValidFromHint: "2024-05-07"},
			},
			want: diagnostic.StructurizerCoverage{TotalFactsSeen: 1, KindFilled: 1, EntitiesFilled: 1, SubjectFilled: 1, ValidFromHintFilled: 1},
		},
		{
			name: "caller_supplied_kind_not_counted",
			before: domain.TemporalFact{
				Kind: domain.KindRelation, Subject: "bob", Entities: []string{"bob"},
				Metadata: map[string]any{MetaValidFromHint: "2024-01-01"},
			},
			after: domain.TemporalFact{
				Kind: domain.KindRelation, Subject: "bob", Entities: []string{"bob"},
				Metadata: map[string]any{MetaValidFromHint: "2024-01-01"},
			},
			want: diagnostic.StructurizerCoverage{TotalFactsSeen: 1},
		},
		{
			name:   "partial_fill_subject_only",
			before: domain.TemporalFact{Kind: domain.KindNote, Entities: []string{"x"}},
			after:  domain.TemporalFact{Kind: domain.KindNote, Subject: "alice", Entities: []string{"x"}},
			want:   diagnostic.StructurizerCoverage{TotalFactsSeen: 1, SubjectFilled: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DiffStructurizerCoverage(tc.before, tc.after)
			if got != tc.want {
				t.Errorf("coverage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDefaultStructurizer_TrustsLLMSuppliedKind verifies that when
// the LLM extractor already chose a Kind via the schema enum, the
// Structurizer's keyword heuristic does NOT second-guess it — even
// if the content text happens to match a different keyword bucket.
// This is the load-bearing assertion for "route 2": Kind ownership
// belongs to the LLM whenever it picked a value.
func TestDefaultStructurizer_TrustsLLMSuppliedKind(t *testing.T) {
	// Content text matches the KindEvent keyword "went to ", but the
	// LLM tagged the fact as state — Structurizer must respect that.
	f := domain.TemporalFact{
		Kind:    domain.KindState,
		Content: "Alice went to live in Paris.",
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
	if out.Kind != domain.KindState {
		t.Errorf("LLM-supplied Kind overwritten by keyword fallback: got %q want %q", out.Kind, domain.KindState)
	}
}

func TestDefaultStructurizer_KeepsCallerSuppliedFields(t *testing.T) {
	// Caller already wrote a fully-formed fact (passthrough path);
	// Structurizer must not overwrite any populated field.
	pre := domain.TemporalFact{
		Kind:      domain.KindRelation,
		Subject:   "Bob",
		Predicate: "spouse",
		Object:    "Carol",
		Entities:  []string{"bob", "carol"},
		Content:   "Bob is married to Carol.",
	}
	out := DefaultStructurizer{}.Structurize(pre, port.IngestInput{})
	if out.Kind != domain.KindRelation || out.Subject != "Bob" || out.Predicate != "spouse" || out.Object != "Carol" {
		t.Errorf("structurizer must not mutate populated fields, got %+v", out)
	}
	if len(out.Entities) != 2 || out.Entities[0] != "bob" || out.Entities[1] != "carol" {
		t.Errorf("entities must not be mutated, got %v", out.Entities)
	}
}

func TestDefaultStructurizer_LiftsSpeakerFromSupportingTurn(t *testing.T) {
	turn := port.TurnContext{
		ID:      "D1:3",
		Speaker: "Caroline",
		Role:    "user",
		Time:    time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC),
		Text:    "I went to the LGBTQ support group.",
	}
	f := domain.TemporalFact{
		Content:      "Caroline went to the LGBTQ support group.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:3"}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	if out.Subject != "Caroline" {
		t.Errorf("subject should be lifted from turn.Speaker, got %q", out.Subject)
	}
	if hint, _ := out.Metadata[MetaValidFromHint].(string); hint == "" {
		t.Errorf("valid_from_hint should be lifted from turn.Time, metadata=%v", out.Metadata)
	}
}

func TestDefaultStructurizer_ExtractsEntitiesFromContent(t *testing.T) {
	f := domain.TemporalFact{Content: "Alice met Bob at the Eiffel Tower in Paris."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})

	want := []string{"alice", "bob", "eiffel", "tower", "paris"}
	got := map[string]bool{}
	for _, e := range out.Entities {
		got[e] = true
	}
	missing := []string{}
	for _, w := range want {
		if !got[w] {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing entities %v, got %v", missing, out.Entities)
	}
}

func TestDefaultStructurizer_FoldsKnownEntityAliases(t *testing.T) {
	// "tom" is the canonical, the snippet writes "Tom" — the
	// snapshot makes the canonical join survive lowercasing /
	// surface variation.
	f := domain.TemporalFact{Content: "Tom recommended a great espresso machine to Alice."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{
		KnownEntities: []port.EntitySnapshot{
			{Canonical: "tom smith", Aliases: []string{"Tom"}},
		},
	})
	found := false
	for _, e := range out.Entities {
		if e == "tom smith" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("known alias should fold into canonical, got %v", out.Entities)
	}
}

func TestDefaultStructurizer_GrepsAbsoluteDateFromContent(t *testing.T) {
	f := domain.TemporalFact{Content: "On 2024-05-07, Alice signed up for the gym."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint != "2024-05-07" {
		t.Errorf("absolute date should be lifted as hint, got %q", hint)
	}
}

func TestNopStructurizer_LeavesFactsUnchanged(t *testing.T) {
	f := domain.TemporalFact{Content: "Alice loves Paris."}
	out := NopStructurizer{}.Structurize(f, port.IngestInput{})
	if out.Kind != "" || len(out.Entities) != 0 || out.Subject != "" {
		t.Errorf("Nop must not fill any field, got %+v", out)
	}
}

// TestDefaultStructurizer_LiftsRelativeTimePhraseFromContent guards
// the when-adapter integration: relative English phrases now
// surface as MetaValidFromHint substrings (e.g. "yesterday")
// without any caller having to plug in a custom parser. The hint
// is consumed by TimeResolver's relative-token table, completing
// the end-to-end path that pure regex parsing could not.
func TestDefaultStructurizer_LiftsRelativeTimePhraseFromContent(t *testing.T) {
	f := domain.TemporalFact{Content: "Alice signed up for the gym yesterday."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint == "" {
		t.Fatalf("expected NL hint for 'yesterday', got empty")
	}
	if !strings.Contains(strings.ToLower(hint), "yesterday") {
		t.Errorf("hint should preserve relative phrase substring, got %q", hint)
	}
}
