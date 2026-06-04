package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/governance"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestCompile_FillsDeterministicFields(t *testing.T) {
	cp := New(Stages{
		IDGen: SequentialIDGenerator("fct_"),
		Clock: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		Facts: []domain.TemporalFact{{
			Kind:      domain.KindRelation,
			Subject:   "Avery",
			Predicate: "spouse",
			Object:    "Rowan",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(res.Facts))
	}
	got := res.Facts[0]
	if got.ID != "fct_000001" {
		t.Errorf("id = %q, want fct_000001", got.ID)
	}
	if got.Scope.RuntimeID != "rt" || got.Scope.UserID != "u1" {
		t.Errorf("scope not propagated: %+v", got.Scope)
	}
	if got.ObservedAt.IsZero() {
		t.Error("observed_at not filled")
	}
	if got.MergeKey != "relation|avery|spouse|rowan" {
		t.Errorf("merge_key = %q, want relation|avery|spouse|rowan", got.MergeKey)
	}
	if got.Confidence != DefaultConfidence {
		t.Errorf("confidence = %v, want %v", got.Confidence, DefaultConfidence)
	}
	// EntityResolver should have added avery/rowan to entities.
	want := map[string]bool{"avery": true, "rowan": true}
	for _, e := range got.Entities {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Errorf("entities missing: %v (have %v)", want, got.Entities)
	}
}

func TestCompile_RecordsExtractorTokenUsage(t *testing.T) {
	client := &fakeLLM{
		Responses: []string{
			`{"segments":[{"segment_id":"t1","families":["semantic_fact"]}]}`,
			`{"proposals":[{
			"text":"Avery adopted a cat.",
			"kind":"event",
			"subject":"Avery",
			"predicate":"adopted",
			"object":"cat",
			"entities":["Avery","cat"],
			"source_ids":["t1"],
			"quote":"Avery adopted a cat."
		}]}`,
		},
		Usages: []llm.TokenUsage{
			{InputTokens: 30, OutputTokens: 10, TotalTokens: 40},
			{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
		},
	}
	cp := New(Stages{
		Extractor: NewLLMExtractor(client),
		IDGen:     SequentialIDGenerator("fct_"),
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Turns: []port.TurnContext{{ID: "t1", Speaker: "Avery", Text: "Avery adopted a cat."}},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "t1",
			Text:          "Avery adopted a cat.",
			Speaker:       "Avery",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("facts = %+v", res.Facts)
	}
	usage := res.ExtractorTokenUsage
	if usage.Calls != 2 || usage.InputTokens != 130 || usage.OutputTokens != 50 || usage.TotalTokens != 180 {
		t.Fatalf("extractor token usage = %+v", usage)
	}
	if usage.AvgTotalTokensPerCall != 90 {
		t.Fatalf("avg total tokens = %v", usage.AvgTotalTokensPerCall)
	}
	if len(usage.Stages) != 2 || usage.Stages[0].Stage != "segment_classifier" || usage.Stages[1].Stage != "semantic_fact" {
		t.Fatalf("stage usage = %+v", usage.Stages)
	}
}

func TestLLMExtractor_GroundingStaysBoundToRoutedSegment(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-temp","families":["parameter_slot"]}]}`,
		`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-1"],
			"quote":"top_p = 0.9",
			"owner":"experiment",
			"name_surface":"top_p",
			"operation_surface":"=",
			"value_surface":"0.9",
			"normalized_value_hint":"0.9",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":""
		}]}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{
			{ObservationID: "obs-1", SpanID: "span-temp", SourceID: "turn-1", Text: "temperature = 0.2"},
			{ObservationID: "obs-1", SpanID: "span-top-p", SourceID: "turn-1", Text: "top_p = 0.9"},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Fatalf("facts = %+v, want none because quote is outside routed segment", res.Facts)
	}
	if res.ExtractorGuard.ByReason["quote_not_in_source"] == 0 {
		t.Fatalf("extractor guard = %+v, want quote_not_in_source", res.ExtractorGuard)
	}
}

func TestLLMExtractor_GroundsWhitespaceAdjacentParameterPair(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-1","families":["parameter_slot"]}]}`,
		`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-1"],
			"quote":"top_p 0.9",
			"owner":"experiment",
			"name_surface":"top_p",
			"operation_surface":"",
			"value_surface":"0.9",
			"normalized_value_hint":"0.9",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":"",
			"confirmation_source_ids":[],
			"confirmation_quote":""
		}]}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "turn-1",
			Text:          "top_p 0.9",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 || res.Facts[0].Kind != domain.KindParameter {
		t.Fatalf("facts = %+v, want one parameter", res.Facts)
	}
}

func TestArbitration_DoesNotUseSemanticOverlapForParameterDuplicates(t *testing.T) {
	baseRef := domain.EvidenceRef{ObservationID: "obs-1", SpanID: "span-1"}
	grounded := []groundedProposal{
		{
			ProposalID:  "p-a",
			Family:      proposalFamilyParameter,
			SupportRefs: []domain.EvidenceRef{baseRef},
			Normalized: groundedNormalizedFields{
				Owner:           "model A",
				CanonicalName:   "temperature",
				ValueKind:       "number",
				NormalizedValue: "0.2",
			},
		},
		{
			ProposalID:  "p-b",
			Family:      proposalFamilyParameter,
			SupportRefs: []domain.EvidenceRef{baseRef},
			Normalized: groundedNormalizedFields{
				Owner:           "model B",
				CanonicalName:   "temperature",
				ValueKind:       "number",
				NormalizedValue: "0.2",
			},
		},
	}
	decision := arbitrateGroundedProposals(grounded)
	if len(decision.Winners) != 2 || len(decision.Losers) != 0 {
		t.Fatalf("arbitration = %+v, want both parameters to win", decision)
	}
}

func TestLLMExtractor_RejectsUngroundedParameterOperationAndCondition(t *testing.T) {
	cases := []struct {
		name       string
		reply      string
		sourceText string
		wantReason string
	}{
		{
			name: "clear operation",
			reply: `{"proposals":[{
				"family":"parameter_slot",
				"source_ids":["turn-1"],
				"quote":"temperature = 0.2",
				"owner":"",
				"name_surface":"temperature",
				"operation_surface":"clear",
				"value_surface":"0.2",
				"normalized_value_hint":"0.2",
				"old_value_surface":"",
				"condition_surface":"",
				"operator_surface":"",
				"effective_time_surface":""
			}]}`,
			sourceText: "temperature = 0.2",
			wantReason: "operation_not_grounded",
		},
		{
			name: "canonical operation not in source",
			reply: `{"proposals":[{
				"family":"parameter_slot",
				"source_ids":["turn-1"],
				"quote":"temperature = 0.2",
				"owner":"",
				"name_surface":"temperature",
				"operation_surface":"set",
				"value_surface":"0.2",
				"normalized_value_hint":"0.2",
				"old_value_surface":"",
				"condition_surface":"",
				"operator_surface":"",
				"effective_time_surface":""
			}]}`,
			sourceText: "temperature = 0.2",
			wantReason: "operation_not_grounded",
		},
		{
			name: "natural language operator",
			reply: `{"proposals":[{
				"family":"parameter_slot",
				"source_ids":["turn-1"],
				"quote":"temperature 等于 0.2",
				"owner":"",
				"name_surface":"temperature",
				"operation_surface":"",
				"value_surface":"0.2",
				"normalized_value_hint":"0.2",
				"old_value_surface":"",
				"condition_surface":"",
				"operator_surface":"等于",
				"effective_time_surface":""
			}]}`,
			sourceText: "temperature 等于 0.2",
			wantReason: "operation_ambiguous",
		},
		{
			name: "condition",
			reply: `{"proposals":[{
				"family":"parameter_slot",
				"source_ids":["turn-1"],
				"quote":"temperature = 0.2",
				"owner":"",
				"name_surface":"temperature",
				"operation_surface":"",
				"value_surface":"0.2",
				"normalized_value_hint":"0.2",
				"old_value_surface":"",
				"condition_surface":"when gpu is enabled",
				"operator_surface":"",
				"effective_time_surface":""
			}]}`,
			sourceText: "temperature = 0.2",
			wantReason: "condition_not_grounded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeLLM{Responses: []string{
				`{"segments":[{"segment_id":"span-1","families":["parameter_slot"]}]}`,
				tc.reply,
			}}
			cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
			res, err := cp.Compile(context.Background(), port.IngestInput{
				Scope: domain.Scope{RuntimeID: "rt"},
				SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
					ObservationID: "obs-1",
					SpanID:        "span-1",
					SourceID:      "turn-1",
					Text:          tc.sourceText,
				}},
			})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if len(res.Facts) != 0 {
				t.Fatalf("facts = %+v, want none", res.Facts)
			}
			if res.ExtractorGuard.ByReason[tc.wantReason] == 0 {
				t.Fatalf("extractor guard = %+v, want %s", res.ExtractorGuard, tc.wantReason)
			}
		})
	}
}

func TestLLMExtractor_ArbitratesStructuredSemanticParameterOverlap(t *testing.T) {
	client := &fakeLLM{ResponsesBySystem: map[string][]string{
		SemanticClassifierSystemPrompt: {`{"segments":[{"segment_id":"span-1","families":["parameter_slot","semantic_fact"]}]}`},
		ParameterProposalSystemPrompt: {`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-1"],
			"quote":"mode = fast",
			"owner":"",
			"name_surface":"mode",
			"operation_surface":"",
			"value_surface":"fast",
			"normalized_value_hint":"fast",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":""
		}]}`},
		SemanticFactProposalSystemPrompt: {`{"proposals":[{
			"family":"semantic_fact",
			"text":"mode is fast",
			"kind":"state",
			"subject":"mode",
			"predicate":"is",
			"object":"fast",
			"entities":["mode"],
			"source_ids":["turn-1"],
			"quote":"mode = fast"
		}]}`},
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "turn-1",
			Text:          "mode = fast",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 || res.Facts[0].Kind != domain.KindParameter {
		t.Fatalf("facts = %+v, want only parameter", res.Facts)
	}
	if res.ExtractorGuard.ByReason["duplicate_by_arbitration"] == 0 {
		t.Fatalf("extractor guard = %+v, want structured parameter/semantic arbitration", res.ExtractorGuard)
	}
}

func TestLLMExtractor_ArbitratesSemanticPredicateWithParameterNameSuffix(t *testing.T) {
	client := &fakeLLM{ResponsesBySystem: map[string][]string{
		SemanticClassifierSystemPrompt: {`{"segments":[{"segment_id":"span-1","families":["parameter_slot","semantic_fact"]}]}`},
		ParameterProposalSystemPrompt: {`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-1"],
			"quote":"temperature = 0.2",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"",
			"effective_time_surface":"",
			"confirmation_source_ids":[],
			"confirmation_quote":""
		}]}`},
		SemanticFactProposalSystemPrompt: {`{"proposals":[{
			"family":"semantic_fact",
			"text":"experiment set temperature = 0.2.",
			"kind":"state",
			"subject":"experiment",
			"predicate":"set temperature",
			"object":"0.2",
			"entities":["experiment","temperature"],
			"source_ids":["turn-1"],
			"quote":"experiment set temperature = 0.2"
		}]}`},
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "turn-1",
			Text:          "experiment set temperature = 0.2",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 || res.Facts[0].Kind != domain.KindParameter {
		t.Fatalf("facts = %+v, want only parameter", res.Facts)
	}
	if res.ExtractorGuard.ByReason["duplicate_by_arbitration"] == 0 {
		t.Fatalf("extractor guard = %+v, want semantic arbitration loser", res.ExtractorGuard)
	}
}

func TestTypedExtractorSpecsReserveProcedureAndIntentFamilies(t *testing.T) {
	for _, spec := range typedExtractorSpecs("test") {
		if spec.Family == proposalFamilyProcedure || spec.Family == proposalFamilyIntentPlan {
			t.Fatalf("reserved family registered in extractor specs: %+v", spec)
		}
	}
	if activeProposalFamily(proposalFamilyProcedure) || activeProposalFamily(proposalFamilyIntentPlan) {
		t.Fatal("reserved families must not be active")
	}
}

func TestLLMExtractor_DoesNotArbitrateUnrelatedSemanticOnSameQuote(t *testing.T) {
	quote := "mode = fast and Alice adopted a cat."
	client := &fakeLLM{ResponsesBySystem: map[string][]string{
		SemanticClassifierSystemPrompt: {`{"segments":[{"segment_id":"span-1","families":["parameter_slot","semantic_fact"]}]}`},
		ParameterProposalSystemPrompt: {`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-1"],
			"quote":"mode = fast and Alice adopted a cat.",
			"owner":"",
			"name_surface":"mode",
			"operation_surface":"",
			"value_surface":"fast",
			"normalized_value_hint":"fast",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":""
		}]}`},
		SemanticFactProposalSystemPrompt: {`{"proposals":[{
			"family":"semantic_fact",
			"text":"Alice adopted a cat.",
			"kind":"event",
			"subject":"Alice",
			"predicate":"adopted",
			"object":"cat",
			"entities":["Alice","cat"],
			"source_ids":["turn-1"],
			"quote":"mode = fast and Alice adopted a cat."
		}]}`},
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "turn-1",
			Text:          quote,
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 2 {
		t.Fatalf("facts = %+v, want parameter and unrelated semantic fact", res.Facts)
	}
}

func TestLLMExtractor_RejectsNonSelfContainedSemanticQuote(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-yes","families":["semantic_fact"]}]}`,
		`{"proposals":[{
			"family":"semantic_fact",
			"text":"The answer is yes.",
			"kind":"state",
			"subject":"",
			"predicate":"",
			"object":"",
			"entities":[],
			"source_ids":["turn-1"],
			"quote":"yes"
		}]}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-yes",
			SourceID:      "turn-1",
			Text:          "yes",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Fatalf("facts = %+v, want none", res.Facts)
	}
	if res.ExtractorGuard.ByReason["non_self_contained_evidence"] == 0 {
		t.Fatalf("extractor guard = %+v, want non_self_contained_evidence", res.ExtractorGuard)
	}
}

func TestLLMExtractor_RejectsSemanticTypedFieldsThatConflictWithBoundText(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-1","families":["semantic_fact"]}]}`,
		`{"proposals":[{
			"family":"semantic_fact",
			"text":"Alice is attending.",
			"kind":"state",
			"subject":"Alice",
			"predicate":"attending",
			"object":"",
			"entities":["Alice"],
			"source_ids":["turn-1"],
			"quote":"Alice is not attending."
		}]}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "turn-1",
			Text:          "Alice is not attending.",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Fatalf("facts = %+v, want none", res.Facts)
	}
	if res.ExtractorGuard.ByReason["text_not_grounded"] == 0 {
		t.Fatalf("extractor guard = %+v, want text_not_grounded", res.ExtractorGuard)
	}
}

func TestParseSemanticFactProposalReplyRejectsMismatchedFamily(t *testing.T) {
	_, err := parseSemanticFactProposalReply([]byte(`{"proposals":[{
		"family":"procedure_step",
		"text":"Do the thing.",
		"kind":"note",
		"subject":"",
		"predicate":"",
		"object":"",
		"entities":[],
		"source_ids":["turn-1"],
		"quote":"Do the thing."
	}]}`), proposalFamilySemanticFact)
	if err == nil {
		t.Fatal("parse err = nil, want family mismatch rejection")
	}
}

func TestLLMExtractor_RecordsSegmentOverflow(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-1","families":["parameter_slot"]}]}`,
		`{"proposals":[],"overflow":true}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{{
			ObservationID: "obs-1",
			SpanID:        "span-1",
			SourceID:      "turn-1",
			Text:          "temperature = 0.2",
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.ExtractorGuard.ByReason["segment_overflow"] == 0 {
		t.Fatalf("extractor guard = %+v, want segment_overflow", res.ExtractorGuard)
	}
}

func TestLLMExtractor_DoesNotUseUnrelatedLaterAffirmativeForConfirmation(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-question","families":["parameter_slot"]}]}`,
		`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-question"],
			"quote":"Confirm temperature = 0.2?",
			"owner":"experiment",
			"name_surface":"temperature",
			"operation_surface":"confirm",
			"value_surface":"0.2",
			"normalized_value_hint":"0.2",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":""
		}]}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{
			{ObservationID: "obs-question", SpanID: "span-question", SourceID: "turn-question", SessionID: "s1", Text: "Confirm temperature = 0.2?", Timestamp: base},
			{ObservationID: "obs-unrelated", SpanID: "span-unrelated", SourceID: "turn-unrelated", SessionID: "s1", Text: "Let's talk about deployment.", Timestamp: base.Add(time.Minute)},
			{ObservationID: "obs-yes", SpanID: "span-yes", SourceID: "turn-yes", SessionID: "s1", Text: "yes", Timestamp: base.Add(2 * time.Minute)},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Fatalf("facts = %+v, want none", res.Facts)
	}
	if res.ExtractorGuard.ByReason["requires_extractable_context"] == 0 {
		t.Fatalf("extractor guard = %+v, want requires_extractable_context", res.ExtractorGuard)
	}
}

func TestParameterProposalSchemaOmitsConfirmationSentimentField(t *testing.T) {
	legacyField := "confirmation_" + "polar" + "ity"
	if strings.Contains(ParameterProposalSchema, legacyField) {
		t.Fatalf("parameter proposal schema must not expose %s: %s", legacyField, ParameterProposalSchema)
	}
}

func TestLLMExtractor_RejectsAmbiguousGenericParameterConfirmation(t *testing.T) {
	client := &fakeLLM{Responses: []string{
		`{"segments":[{"segment_id":"span-mode","families":["parameter_slot"]}]}`,
		`{"proposals":[{
			"family":"parameter_slot",
			"source_ids":["turn-mode"],
			"quote":"Confirm mode = fast?",
			"owner":"experiment",
			"name_surface":"mode",
			"operation_surface":"Confirm",
			"value_surface":"fast",
			"normalized_value_hint":"fast",
			"old_value_surface":"",
			"condition_surface":"",
			"operator_surface":"=",
			"effective_time_surface":""
		}]}`,
	}}
	cp := New(Stages{Extractor: NewLLMExtractor(client), IDGen: SequentialIDGenerator("fct_")})
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		SourceEvidenceSpans: []domain.SourceEvidenceSpan{
			{ObservationID: "obs-mode", SpanID: "span-mode", SourceID: "turn-mode", SessionID: "s1", Text: "Confirm mode = fast?", Timestamp: base},
			{ObservationID: "obs-top-k", SpanID: "span-top-k", SourceID: "turn-top-k", SessionID: "s1", Text: "Confirm top_k = 50?", Timestamp: base.Add(time.Second)},
			{ObservationID: "obs-yes", SpanID: "span-yes", SourceID: "turn-yes", SessionID: "s1", Text: "是的", Timestamp: base.Add(2 * time.Second)},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Fatalf("facts = %+v, want none", res.Facts)
	}
	if res.ExtractorGuard.ByReason["requires_extractable_context"] == 0 {
		t.Fatalf("extractor guard = %+v, want requires_extractable_context", res.ExtractorGuard)
	}
}

func TestCompile_RelationMergeKeyDifferentiatesObjects(t *testing.T) {
	cp := New(Stages{IDGen: SequentialIDGenerator("f")})
	mk := func(object string) string {
		res, err := cp.Compile(context.Background(), port.IngestInput{
			Scope: domain.Scope{RuntimeID: "rt"},
			Facts: []domain.TemporalFact{{
				Kind:      domain.KindRelation,
				Subject:   "Avery",
				Predicate: "spouse",
				Object:    object,
			}},
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return res.Facts[0].MergeKey
	}
	a := mk("Rowan")
	b := mk("Morgan")
	if a == b {
		t.Fatalf("relation merge keys must differ by object; got %q for both", a)
	}
}

func TestCompile_StateMergeKeyDedupes(t *testing.T) {
	cp := New(Stages{IDGen: SequentialIDGenerator("f")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{
			{Kind: domain.KindState, Subject: "Avery", Predicate: "city", Content: "Riverton"},
			{Kind: domain.KindState, Subject: "avery", Predicate: "CITY", Content: "Berlin"},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 2 {
		t.Fatalf("want 2 facts, got %d", len(res.Facts))
	}
	if res.Facts[0].MergeKey != res.Facts[1].MergeKey {
		t.Errorf("normalized state merge keys should match: %q vs %q",
			res.Facts[0].MergeKey, res.Facts[1].MergeKey)
	}
}

func TestCompile_ParameterForcesCanonicalMergeKey(t *testing.T) {
	cp := New(Stages{IDGen: SequentialIDGenerator("f")})
	fact := domain.TemporalFact{
		Kind:     domain.KindParameter,
		Subject:  "experiment",
		Object:   "0.2",
		MergeKey: "caller-controlled",
		EvidenceRefs: []domain.EvidenceRef{{
			ID:            "turn-1",
			ObservationID: "obs-1",
			SpanID:        "span-1",
			Text:          "temperature = 0.2",
		}},
		Metadata: map[string]any{
			domain.MetaParameterOwner:           "experiment",
			domain.MetaParameterCanonicalName:   "temperature",
			domain.MetaParameterValueKind:       "number",
			domain.MetaParameterNormalizedValue: "0.2",
		},
	}
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{fact},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("facts = %+v, want one", res.Facts)
	}
	if res.Facts[0].MergeKey == "caller-controlled" {
		t.Fatalf("parameter merge key used caller authority")
	}
	if res.Facts[0].MergeKey != DefaultMergeKey(res.Facts[0]) {
		t.Fatalf("merge_key = %q, want %q", res.Facts[0].MergeKey, DefaultMergeKey(res.Facts[0]))
	}
}

func TestCompile_ParameterClearUsesCanonicalSentinel(t *testing.T) {
	cp := New(Stages{IDGen: SequentialIDGenerator("f")})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{
			Kind:    domain.KindParameter,
			Subject: "experiment",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "turn-1",
				ObservationID: "obs-1",
				SpanID:        "span-1",
				Text:          "clear temperature",
			}},
			Metadata: map[string]any{
				domain.MetaParameterOwner:         "experiment",
				domain.MetaParameterCanonicalName: "temperature",
				domain.MetaParameterOperation:     "clear",
			},
		}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := res.Facts[0].Metadata[domain.MetaParameterNormalizedValue]; got != clearedParameterValue {
		t.Fatalf("normalized clear value = %v, want %q", got, clearedParameterValue)
	}
}

func TestCompile_RejectsInvalidKind(t *testing.T) {
	cp := Default()
	_, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: "ufo", Content: "x"}},
	})
	if err == nil {
		t.Fatal("want error for invalid kind")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("invalid kind should map to Validation: %v", err)
	}
}

func TestCompile_RequiresScope_IsValidation(t *testing.T) {
	cp := Default()
	_, err := cp.Compile(context.Background(), port.IngestInput{
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "x"}},
	})
	if err == nil {
		t.Fatal("want error for missing scope")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("missing scope should map to Validation: %v", err)
	}
}

func TestCompile_PolicyRejectDrops(t *testing.T) {
	cp := New(Stages{
		IDGen:  SequentialIDGenerator("f"),
		Policy: rejectAllPolicy{},
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "secret"}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 0 {
		t.Errorf("want 0 facts after policy reject, got %d", len(res.Facts))
	}
	if len(res.Dropped) != 1 {
		t.Errorf("want 1 dropped fact, got %d", len(res.Dropped))
	}
}

func TestCompile_GovernanceMutationPrecedesDerivedFields(t *testing.T) {
	cp := New(Stages{
		IDGen: SequentialIDGenerator("f"),
		Governance: &governance.Governance{
			Write: mutateContentPolicy{content: "redacted content"},
		},
	})
	res, err := cp.Compile(context.Background(), port.IngestInput{
		Scope: domain.Scope{RuntimeID: "rt"},
		Facts: []domain.TemporalFact{{Kind: domain.KindNote, Content: "secret content"}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(res.Facts))
	}
	got := res.Facts[0]
	if got.Content != "redacted content" {
		t.Fatalf("content = %q", got.Content)
	}
	if got.MergeKey != DefaultMergeKey(got) {
		t.Fatalf("merge_key = %q, want derived from mutated fact %q", got.MergeKey, DefaultMergeKey(got))
	}
}

type rejectAllPolicy struct{}

func (rejectAllPolicy) Apply(f domain.TemporalFact) (domain.TemporalFact, bool) { return f, false }

type mutateContentPolicy struct {
	content string
}

func (p mutateContentPolicy) Apply(f domain.TemporalFact) (domain.TemporalFact, bool) {
	f.Content = p.content
	return f, true
}
