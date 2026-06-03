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
// f.Kind, the Structurizer's fallback is KindNote. The LLM owns normal
// classification; this test guards against reintroducing Go keyword inference
// for event/state/preference/relation/plan/procedure.
func TestDefaultStructurizer_KindFallbackDefaultsToNote(t *testing.T) {
	cases := []string{
		"Avery loves black coffee in the morning.",
		"Avery plans to visit Riverton next month.",
		"Avery is married to Rowan.",
		"Avery lives in San Francisco.",
		"Avery went to the cinema yesterday.",
		"When comparing options, use a markdown table.",
		"Before processing invoices, run OCR and then extract entities.",
		"Always use markdown tables for comparisons.",
		"First run OCR, then extract invoice entities.",
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
	f := domain.TemporalFact{Content: "Avery met Rowan at Riverton."}
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
				Kind: domain.KindState, Subject: "avery", Entities: []string{"avery"},
				Metadata: map[string]any{MetaValidFromHint: "2024-05-07"},
			},
			want: diagnostic.StructurizerCoverage{TotalFactsSeen: 1, KindFilled: 1, EntitiesFilled: 1, SubjectFilled: 1, ValidFromHintFilled: 1},
		},
		{
			name: "caller_supplied_kind_not_counted",
			before: domain.TemporalFact{
				Kind: domain.KindRelation, Subject: "rowan", Entities: []string{"rowan"},
				Metadata: map[string]any{MetaValidFromHint: "2024-01-01"},
			},
			after: domain.TemporalFact{
				Kind: domain.KindRelation, Subject: "rowan", Entities: []string{"rowan"},
				Metadata: map[string]any{MetaValidFromHint: "2024-01-01"},
			},
			want: diagnostic.StructurizerCoverage{TotalFactsSeen: 1},
		},
		{
			name:   "partial_fill_subject_only",
			before: domain.TemporalFact{Kind: domain.KindNote, Entities: []string{"x"}},
			after:  domain.TemporalFact{Kind: domain.KindNote, Subject: "avery", Entities: []string{"x"}},
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
		Content: "Avery went to live in Riverton.",
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
		Subject:   "Rowan",
		Predicate: "spouse",
		Object:    "Morgan",
		Entities:  []string{"rowan", "carol"},
		Content:   "Rowan is married to Morgan.",
	}
	out := DefaultStructurizer{}.Structurize(pre, port.IngestInput{})
	if out.Kind != domain.KindRelation || out.Subject != "Rowan" || out.Predicate != "spouse" || out.Object != "Morgan" {
		t.Errorf("structurizer must not mutate populated fields, got %+v", out)
	}
	if len(out.Entities) != 2 || out.Entities[0] != "rowan" || out.Entities[1] != "carol" {
		t.Errorf("entities must not be mutated, got %v", out.Entities)
	}
}

func TestDefaultStructurizer_LiftsSpeakerFromSupportingTurn(t *testing.T) {
	turn := port.TurnContext{
		ID:      "D1:3",
		Speaker: "Rhea",
		Role:    "user",
		Time:    time.Date(2024, 5, 7, 9, 0, 0, 0, time.UTC),
		Text:    "I went to the neighborhood art workshop.",
	}
	f := domain.TemporalFact{
		Content:      "Rhea went to the neighborhood art workshop.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:3"}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	if out.Subject != "Rhea" {
		t.Errorf("subject should be lifted from turn.Speaker, got %q", out.Subject)
	}
	if hint, _ := out.Metadata[MetaValidFromHint].(string); hint == "" {
		t.Errorf("valid_from_hint should be lifted from turn.Time, metadata=%v", out.Metadata)
	}
	if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceTimeFallback {
		t.Errorf("turn time fallback source = %q, want %q", source, ValidFromSourceTimeFallback)
	}
}

func TestDefaultStructurizer_DoesNotFillSuppressedSubjectFromSpeaker(t *testing.T) {
	turn := port.TurnContext{
		ID:      "D1:3",
		Speaker: "Rowan",
		Role:    "assistant",
		Text:    "Avery likes Riverton.",
	}
	f := domain.TemporalFact{
		Content:      "Avery likes Riverton.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:3"}},
		Metadata:     map[string]any{domain.MetaSubjectSuppressed: true},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	if out.Subject != "" {
		t.Fatalf("suppressed subject should not be filled from speaker, got %q", out.Subject)
	}
}

func TestDefaultStructurizer_PrefersContentRelativeTimeOverTurnTime(t *testing.T) {
	turn := port.TurnContext{
		ID:      "D1:4",
		Speaker: "Rhea",
		Role:    "user",
		Time:    time.Date(2023, 6, 27, 10, 37, 0, 0, time.UTC),
		Text:    "I moved from Norland 4 years ago.",
	}
	f := domain.TemporalFact{
		Content:      "Rhea moved from Norland 4 years ago.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:4"}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint == "" {
		t.Fatalf("expected relative content hint, metadata=%v", out.Metadata)
	}
	at, _ := out.Metadata[MetaValidFromAt].(string)
	parsed, ok := parseTimeHint(at, time.Time{}, false)
	if !ok {
		t.Fatalf("expected parsed valid_from_at, got hint=%q at=%q metadata=%v", hint, at, out.Metadata)
	}
	want := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Errorf("ValidFromAt = %v, want %v (hint=%q at=%q)", parsed, want, hint, at)
	}
	if timexValue, _ := out.Metadata[MetaValidFromTimex].(string); timexValue != "2019" {
		t.Errorf("valid_from_timex = %q, want 2019", timexValue)
	}
	if precision, _ := out.Metadata[MetaValidFromPrec].(string); precision != "year" {
		t.Errorf("valid_from_precision = %q, want year", precision)
	}
	if to, _ := out.Metadata[MetaValidToAt].(string); to == "" {
		t.Errorf("expected valid_to_at from relative year range, metadata=%v", out.Metadata)
	}
	if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceContentRelative {
		t.Errorf("relative content source = %q, want %q", source, ValidFromSourceContentRelative)
	}
}

func TestDefaultStructurizer_LiftsLexicalRelativeTimeOverTurnTime(t *testing.T) {
	cases := []struct {
		name    string
		content string
		turnAt  time.Time
		wantAt  time.Time
		wantRaw string
	}{
		{
			name:    "last year",
			content: "Juno painted a lake sunrise last year.",
			turnAt:  time.Date(2023, 5, 8, 13, 56, 0, 0, time.UTC),
			wantAt:  time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC),
			wantRaw: "last year",
		},
		{
			name:    "next month",
			content: "Eli is hosting a dance competition next month.",
			turnAt:  time.Date(2023, 4, 3, 13, 26, 0, 0, time.UTC),
			wantAt:  time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC),
			wantRaw: "next month",
		},
		{
			name:    "last month",
			content: "John and his colleagues attended a convention last month.",
			turnAt:  time.Date(2023, 4, 18, 19, 34, 0, 0, time.UTC),
			wantAt:  time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC),
			wantRaw: "last month",
		},
		{
			name:    "two weekends ago",
			content: "Juno went camping with her family two weekends ago.",
			turnAt:  time.Date(2023, 7, 17, 14, 31, 0, 0, time.UTC),
			wantAt:  time.Date(2023, 7, 8, 0, 0, 0, 0, time.UTC),
			wantRaw: "two weekends ago",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := port.TurnContext{ID: "D1:1", Time: tc.turnAt}
			f := domain.TemporalFact{
				Content:      tc.content,
				EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1"}},
			}
			out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
			hint, _ := out.Metadata[MetaValidFromHint].(string)
			if hint != tc.wantRaw {
				t.Fatalf("hint = %q, want %q (metadata=%v)", hint, tc.wantRaw, out.Metadata)
			}
			at, _ := out.Metadata[MetaValidFromAt].(string)
			parsed, ok := parseTimeHint(at, time.Time{}, false)
			if !ok || !parsed.Equal(tc.wantAt) {
				t.Fatalf("ValidFromAt = %v ok=%v, want %v (hint=%q at=%q metadata=%v)", parsed, ok, tc.wantAt, hint, at, out.Metadata)
			}
			if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceContentRelative {
				t.Fatalf("relative content source = %q, want %q", source, ValidFromSourceContentRelative)
			}
		})
	}
}

func TestDefaultStructurizer_PrefersEvidenceRelativeTimeOverContentDate(t *testing.T) {
	turn := port.TurnContext{
		ID:   "D1:1",
		Time: time.Date(2024, 5, 8, 9, 0, 0, 0, time.UTC),
		Text: "I visited the observatory yesterday.",
	}
	f := domain.TemporalFact{
		Kind:         domain.KindEvent,
		Content:      "On 2024-05-08, Avery visited the observatory yesterday.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1", Text: "I visited the observatory yesterday."}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint != "yesterday" {
		t.Fatalf("hint = %q, want evidence relative phrase; metadata=%v", hint, out.Metadata)
	}
	at, _ := out.Metadata[MetaValidFromAt].(string)
	parsed, ok := parseTimeHint(at, time.Time{}, false)
	want := time.Date(2024, 5, 7, 0, 0, 0, 0, time.UTC)
	if !ok || !parsed.Equal(want) {
		t.Fatalf("ValidFromAt = %v ok=%v, want %v (at=%q metadata=%v)", parsed, ok, want, at, out.Metadata)
	}
}

func TestDefaultStructurizer_EvidenceRelativeTimeDoesNotRewriteModality(t *testing.T) {
	turn := port.TurnContext{
		ID:   "D1:1",
		Time: time.Date(2024, 5, 8, 9, 0, 0, 0, time.UTC),
		Text: "I plan to visit the observatory next month.",
	}
	f := domain.TemporalFact{
		Kind:         domain.KindPlan,
		Content:      "On 2024-05-08, Avery plans to visit the observatory next month.",
		Modality:     domain.ModalityActual,
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1", Text: "I plan to visit the observatory next month."}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint != "next month" {
		t.Fatalf("hint = %q, want next month; metadata=%v", hint, out.Metadata)
	}
	if out.Modality != domain.ModalityActual {
		t.Fatalf("structurizer must not infer modality, got %q", out.Modality)
	}
}

func TestDefaultStructurizer_DoesNotUseEvidenceRelativeTimeForNotes(t *testing.T) {
	turn := port.TurnContext{
		ID:   "D1:1",
		Time: time.Date(2024, 5, 8, 9, 0, 0, 0, time.UTC),
		Text: "It feels like yesterday when I wore that costume.",
	}
	f := domain.TemporalFact{
		Kind:         domain.KindNote,
		Content:      "On 2024-05-08, Avery said it feels like yesterday when Avery wore that costume.",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1", Text: "It feels like yesterday when I wore that costume."}},
	}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	if hint, _ := out.Metadata[MetaValidFromHint].(string); hint != "2024-05-08" {
		t.Fatalf("note should use content/source date fallback, got hint=%q metadata=%v", hint, out.Metadata)
	}
}

func TestDefaultStructurizer_DoesNotPromoteDurationOrSetToEventTime(t *testing.T) {
	turn := port.TurnContext{
		ID:   "D1:1",
		Time: time.Date(2023, 7, 20, 20, 56, 0, 0, time.UTC),
	}
	cases := []string{
		"Juno's family usually goes to the beach once or twice a year.",
		"Juno has been creating art for seven years.",
	}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			f := domain.TemporalFact{
				Content:      content,
				EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1"}},
			}
			out := DefaultStructurizer{}.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
			if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceTimeFallback {
				t.Fatalf("valid_from_source = %q, want fallback; metadata=%v", source, out.Metadata)
			}
			if hint, _ := out.Metadata[MetaValidFromHint].(string); hint != turn.Time.UTC().Format(time.RFC3339Nano) {
				t.Fatalf("valid_from_hint = %q, want turn time fallback; metadata=%v", hint, out.Metadata)
			}
			if kind, _ := out.Metadata[MetaValidFromKind].(string); kind != "date" {
				t.Fatalf("valid_from_kind = %q, want fallback date; metadata=%v", kind, out.Metadata)
			}
		})
	}
}

func TestDefaultStructurizer_CarriesParsedTimeFromCustomParser(t *testing.T) {
	want := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	s := DefaultStructurizer{TimeParser: stubTimeParser{
		match: &timex.Match{Text: "四年前", Time: time.Date(2019, 6, 27, 0, 0, 0, 0, time.UTC), Index: 10},
	}}
	turn := port.TurnContext{ID: "D1:1", Time: time.Date(2023, 6, 27, 9, 0, 0, 0, time.UTC)}
	f := domain.TemporalFact{
		Content:      "Rhea 四年前从诺兰搬来。",
		EvidenceRefs: []domain.EvidenceRef{{ID: "D1:1"}},
	}
	out := s.Structurize(f, port.IngestInput{Turns: []port.TurnContext{turn}})
	if hint, _ := out.Metadata[MetaValidFromHint].(string); hint != "四年前" {
		t.Fatalf("raw multilingual hint = %q, want 四年前", hint)
	}
	at, _ := out.Metadata[MetaValidFromAt].(string)
	parsed, ok := parseTimeHint(at, time.Time{}, false)
	if !ok || !parsed.Equal(want) {
		t.Fatalf("parsed multilingual time = %v ok=%v, want %v (metadata=%v)", parsed, ok, want, out.Metadata)
	}
	if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceContentRelative {
		t.Fatalf("multilingual relative source = %q, want %q", source, ValidFromSourceContentRelative)
	}
}

func TestDefaultStructurizer_ExtractsEntitiesFromContent(t *testing.T) {
	f := domain.TemporalFact{Content: "Avery met Rowan at the Old Clocktower in Riverton."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})

	want := []string{"avery", "rowan", "old", "clocktower", "riverton"}
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

func TestDefaultStructurizer_DropsStructuralStopwordsFromEntities(t *testing.T) {
	f := domain.TemporalFact{Content: "On 2024-05-07, Avery signed up for woodworking. Taking time for herself helps Avery."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})

	for _, entity := range out.Entities {
		if entity == "on" {
			t.Fatalf("structural stopword should be dropped, got %v", out.Entities)
		}
	}
}

func TestDefaultStructurizer_FoldsKnownEntityAliases(t *testing.T) {
	// "tom" is the canonical, the snippet writes "Tom" — the
	// snapshot makes the canonical join survive lowercasing /
	// surface variation.
	f := domain.TemporalFact{Content: "Tom recommended a great espresso machine to Avery."}
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

func TestDefaultStructurizer_KnownEntityAliasRequiresTokenBoundary(t *testing.T) {
	f := domain.TemporalFact{Content: "The cart arrived after dinner."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{
		KnownEntities: []port.EntitySnapshot{
			{Canonical: "art collective", Aliases: []string{"art"}},
		},
	})
	for _, e := range out.Entities {
		if e == "art collective" {
			t.Fatalf("known alias should not match inside unrelated token, got %v", out.Entities)
		}
	}
}

func TestDefaultStructurizer_GrepsAbsoluteDateFromContent(t *testing.T) {
	f := domain.TemporalFact{Content: "On 2024-05-07, Avery signed up for the gym."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint != "2024-05-07" {
		t.Errorf("absolute date should be lifted as hint, got %q", hint)
	}
	if source, _ := out.Metadata[MetaValidFromSource].(string); source != ValidFromSourceContentExplicit {
		t.Errorf("absolute date source = %q, want %q", source, ValidFromSourceContentExplicit)
	}
}

func TestNopStructurizer_LeavesFactsUnchanged(t *testing.T) {
	f := domain.TemporalFact{Content: "Avery loves Riverton."}
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
	f := domain.TemporalFact{Content: "Avery signed up for the gym yesterday."}
	out := DefaultStructurizer{}.Structurize(f, port.IngestInput{})
	hint, _ := out.Metadata[MetaValidFromHint].(string)
	if hint == "" {
		t.Fatalf("expected NL hint for 'yesterday', got empty")
	}
	if !strings.Contains(strings.ToLower(hint), "yesterday") {
		t.Errorf("hint should preserve relative phrase substring, got %q", hint)
	}
}
