package ingest

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/text/timex"
)

// TestDefaultStructurizer_KindFallbackDefaultsToNote pins the post-route-2
// contract: when neither the caller nor the LLM extractor populated
// f.Kind, the Structurizer's fallback is KindNote except for narrow
// procedural-memory patterns. The LLM still owns normal
// classification; this test guards against reintroducing broad
// keyword inference for event/state/preference/relation/plan.
func TestDefaultStructurizer_KindFallbackDefaultsToNote(t *testing.T) {
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

func TestDefaultStructurizer_KindFallbackDetectsProcedure(t *testing.T) {
	cases := []string{
		"When comparing options, use a markdown table.",
		"Before processing invoices, run OCR and then extract entities.",
		"Always use markdown tables for comparisons.",
		"First run OCR, then extract invoice entities.",
	}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			f := domain.TemporalFact{Content: content}
			out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
			if out.Kind != domain.KindProcedure {
				t.Errorf("kind = %q, want KindProcedure", out.Kind)
			}
		})
	}

	f := domain.TemporalFact{Content: "Alice prefers tea in the morning."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
	if out.Kind != domain.KindNote {
		t.Errorf("simple preference text should stay Note fallback, got %q", out.Kind)
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

type stubTimeParser struct {
	match *timex.Match
}

func (s stubTimeParser) Parse(string, time.Time) (*timex.Match, error) {
	return s.match, nil
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

func TestDefaultStructurizer_PrefersContentRelativeTimeOverTurnTime(t *testing.T) {
	turn := port.TurnContext{
		ID:      "D1:4",
		Speaker: "Caroline",
		Role:    "user",
		Time:    time.Date(2023, 6, 27, 10, 37, 0, 0, time.UTC),
		Text:    "I moved from Sweden 4 years ago.",
	}
	f := domain.TemporalFact{
		Content:      "Caroline moved from Sweden 4 years ago.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:4"}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint == "" {
		t.Fatalf("expected relative content hint, metadata=%v", out.Metadata)
	}
	at, _ := out.Metadata[MetaValidFromAt].(string)
	parsed, ok := parseAbsoluteTime(at)
	if !ok {
		t.Fatalf("expected parsed valid_from_at, got hint=%q at=%q metadata=%v", hint, at, out.Metadata)
	}
	want := time.Date(2019, 6, 27, 10, 37, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Errorf("ValidFromAt = %v, want %v (hint=%q at=%q)", parsed, want, hint, at)
	}
}

func TestDefaultStructurizer_CarriesParsedTimeFromCustomParser(t *testing.T) {
	want := time.Date(2019, 6, 27, 0, 0, 0, 0, time.UTC)
	s := DefaultStructurizer{TimeParser: stubTimeParser{
		match: &timex.Match{Text: "四年前", Time: want, Index: 10},
	}}
	turn := port.TurnContext{ID: "D1:1", Time: time.Date(2023, 6, 27, 9, 0, 0, 0, time.UTC)}
	f := domain.TemporalFact{
		Content:      "Caroline 四年前从瑞典搬来。",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1"}},
	}
	out := s.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	if hint, _ := out.Metadata[MetaValidFromHint].(string); hint != "四年前" {
		t.Fatalf("raw multilingual hint = %q, want 四年前", hint)
	}
	at, _ := out.Metadata[MetaValidFromAt].(string)
	parsed, ok := parseAbsoluteTime(at)
	if !ok || !parsed.Equal(want) {
		t.Fatalf("parsed multilingual time = %v ok=%v, want %v (metadata=%v)", parsed, ok, want, out.Metadata)
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
