package stages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/graphledger"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type linkStoreRecorder struct {
	appendErr error
	appended  []domain.FactLink
	deleted   []string
	byNode    map[string][]domain.FactLink
}

func (s *linkStoreRecorder) Append(_ context.Context, links []domain.FactLink) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.appended = append(s.appended, links...)
	return nil
}

func (s *linkStoreRecorder) Get(context.Context, domain.Scope, string) (domain.FactLink, error) {
	return domain.FactLink{}, port.ErrNotFound
}

func (s *linkStoreRecorder) List(context.Context, domain.Scope, port.LinkListQuery) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *linkStoreRecorder) FindByNode(_ context.Context, _ domain.Scope, node domain.GraphNodeRef) ([]domain.FactLink, error) {
	if s.byNode == nil {
		return nil, nil
	}
	return append([]domain.FactLink(nil), s.byNode[string(node.Kind)+"|"+node.ID]...), nil
}

func (s *linkStoreRecorder) FindByMergeKey(context.Context, domain.Scope, string) ([]domain.FactLink, error) {
	return nil, nil
}

func (s *linkStoreRecorder) Delete(_ context.Context, _ domain.Scope, ids []string) error {
	s.deleted = append(s.deleted, ids...)
	return nil
}

func (s *linkStoreRecorder) DeleteByNode(context.Context, domain.Scope, domain.GraphNodeRef) (int, error) {
	return 0, nil
}

func (s *linkStoreRecorder) DeleteByScope(context.Context, domain.Scope) (int, error) {
	return 0, nil
}

func (s *linkStoreRecorder) Close() error { return nil }

func TestCommitGraph_DoesNotSynthesizeObservationsForBareEvidenceRefs(t *testing.T) {
	boom := errors.New("links down")
	observations := &observationStoreRecorder{}
	projection := &observationProjectionRecorder{}
	links := &linkStoreRecorder{appendErr: boom}
	stage := stages.NewCommitGraph(observations, links, projection)
	state := &write.WriteState{
		Scope: domain.Scope{RuntimeID: "rt", UserID: "u1"},
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:      "fact-1",
			Scope:   domain.Scope{RuntimeID: "rt", UserID: "u1"},
			Content: "Alice likes tea",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:        "ev-1",
				MessageID: "msg-1",
				Text:      "Alice likes tea",
			}},
		}}},
	}

	_, err := stage.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	if len(observations.appended) != 0 {
		t.Fatalf("bare evidence refs must not synthesize observations, got %+v", observations.appended)
	}
	if len(projection.projected) != 0 || len(projection.forgotten) != 0 {
		t.Fatalf("bare evidence refs must not touch observation projection, projected=%+v forgotten=%+v", projection.projected, projection.forgotten)
	}
	if len(observations.deleted) != 0 {
		t.Fatalf("bare evidence refs must not schedule observation cleanup, deleted=%v", observations.deleted)
	}
}

func TestCommitGraph_LinkFailureRestoresExistingObservationSnapshot(t *testing.T) {
	boom := errors.New("links down")
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	existing := domain.Observation{
		ID:       "obs-existing",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		SourceID: "ev-1",
		Text:     "old text",
		Spans: []domain.ObservationSpan{{
			ID:            "span-old",
			ObservationID: "obs-existing",
			SourceID:      "ev-1",
			Kind:          domain.ObservationSpanKindQuote,
			Text:          "old text",
		}},
	}
	observations := &observationStoreRecorder{getByID: map[string]domain.Observation{
		existing.ID: existing,
	}}
	projection := &observationProjectionRecorder{}
	links := &linkStoreRecorder{appendErr: boom}
	stage := stages.NewCommitGraph(observations, links, projection)
	state := &write.WriteState{
		Scope: scope,
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:      "fact-1",
			Scope:   scope,
			Content: "Alice likes tea",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "ev-1",
				MessageID:     "msg-1",
				Text:          "new text",
				ObservationID: existing.ID,
				SpanID:        "span-new",
			}},
		}}},
	}

	_, err := stage.Run(context.Background(), state)
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	if len(observations.deleted) != 0 {
		t.Fatalf("existing observation should not be touched, deleted=%v", observations.deleted)
	}
	if len(observations.appended) != 0 {
		t.Fatalf("existing observation should not be re-appended, got %+v", observations.appended)
	}
	if len(projection.forgotten) != 0 {
		t.Fatalf("existing observation projection should not be forgotten, got %v", projection.forgotten)
	}
	if len(projection.projected) != 0 {
		t.Fatalf("existing observation should not be re-projected, got %+v", projection.projected)
	}
}

func TestCommitGraph_ParameterRequiresGraphDependencies(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	state := &write.WriteState{
		Scope: scope,
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:      "param-1",
			Scope:   scope,
			Kind:    domain.KindParameter,
			Content: "experiment has parameter temperature set to 0.2.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:            "src-1",
				ObservationID: "obs-1",
				SpanID:        "span-1",
				Text:          "temperature 0.2",
			}},
		}}},
	}
	stage := stages.NewCommitGraph(&observationStoreRecorder{}, nil, nil)
	skip, _ := stage.Skip(context.Background(), state)
	if skip {
		t.Fatal("parameter facts must not skip commit_graph when dependencies are missing")
	}
	_, err := stage.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run err = nil, want graph dependency error")
	}
}

func TestGraphDependencies_RejectsNonExtractableParameterObservation(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	base := domain.Observation{
		ID:    "obs-1",
		Scope: scope,
		Kind:  domain.ObservationKindTurn,
		Text:  "mode = fast",
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			Text:          "mode = fast",
			Start:         0,
			End:           len("mode = fast"),
		}},
	}
	cases := []struct {
		name string
		obs  domain.Observation
	}{
		{name: "expired", obs: func() domain.Observation { o := base; o.Metadata = map[string]any{"expired": true}; return o }()},
		{name: "hard deleted", obs: func() domain.Observation { o := base; o.Metadata = map[string]any{"hard_deleted": true}; return o }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := stages.NewGraphDependencies(&observationStoreRecorder{getByID: map[string]domain.Observation{"obs-1": tc.obs}}, &linkStoreRecorder{})
			state := &write.WriteState{
				Scope: scope,
				Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
					ID:    "fact-1",
					Scope: scope,
					Kind:  domain.KindParameter,
					EvidenceRefs: []domain.EvidenceRef{{
						ObservationID: "obs-1",
						SpanID:        "span-1",
						Text:          "mode = fast",
					}},
				}}},
			}
			_, err := stage.Run(context.Background(), state)
			if err == nil {
				t.Fatal("Run err = nil, want graph dependency validation error")
			}
		})
	}
}

func TestGraphDependencies_RejectsUnsupportedParameterEvidenceText(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	stage := stages.NewGraphDependencies(&observationStoreRecorder{getByID: map[string]domain.Observation{"obs-1": {
		ID:       "obs-1",
		Scope:    scope,
		Kind:     domain.ObservationKindTurn,
		SourceID: "turn-1",
		Text:     "favorite color = blue",
		Spans: []domain.ObservationSpan{{
			ID:            "span-1",
			ObservationID: "obs-1",
			SourceID:      "turn-1",
			Text:          "favorite color = blue",
			Start:         0,
			End:           len("favorite color = blue"),
		}},
	}}}, &linkStoreRecorder{})
	state := &write.WriteState{
		Scope: scope,
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:     "fact-1",
			Scope:  scope,
			Kind:   domain.KindParameter,
			Object: "0.2",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-1",
				SpanID:        "span-1",
				Text:          "favorite color = blue",
			}},
			Metadata: map[string]any{
				domain.MetaParameterOwner:           "conversation",
				domain.MetaParameterCanonicalName:   "temperature",
				domain.MetaParameterNameSurface:     "temperature",
				domain.MetaParameterOperation:       "set",
				domain.MetaParameterValueKind:       "number",
				domain.MetaParameterRawValue:        "0.2",
				domain.MetaParameterNormalizedValue: "0.2",
			},
		}}},
	}
	_, err := stage.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run err = nil, want unsupported parameter evidence text rejection")
	}
}

func TestGraphDependencies_RejectsParameterEvidenceSplitAcrossSpans(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	stage := stages.NewGraphDependencies(&observationStoreRecorder{getByID: map[string]domain.Observation{
		"obs-name": {
			ID:       "obs-name",
			Scope:    scope,
			Kind:     domain.ObservationKindTurn,
			SourceID: "turn-name",
			Text:     "temperature",
			Spans: []domain.ObservationSpan{{
				ID:            "span-name",
				ObservationID: "obs-name",
				SourceID:      "turn-name",
				Text:          "temperature",
				Start:         0,
				End:           len("temperature"),
			}},
		},
		"obs-value": {
			ID:       "obs-value",
			Scope:    scope,
			Kind:     domain.ObservationKindTurn,
			SourceID: "turn-value",
			Text:     "0.2",
			Spans: []domain.ObservationSpan{{
				ID:            "span-value",
				ObservationID: "obs-value",
				SourceID:      "turn-value",
				Text:          "0.2",
				Start:         0,
				End:           len("0.2"),
			}},
		},
	}}, &linkStoreRecorder{})
	state := &write.WriteState{
		Scope: scope,
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:     "fact-1",
			Scope:  scope,
			Kind:   domain.KindParameter,
			Object: "0.2",
			EvidenceRefs: []domain.EvidenceRef{
				{ObservationID: "obs-name", SpanID: "span-name", Text: "temperature"},
				{ObservationID: "obs-value", SpanID: "span-value", Text: "0.2"},
			},
			Metadata: map[string]any{
				domain.MetaParameterOwner:           "experiment",
				domain.MetaParameterCanonicalName:   "temperature",
				domain.MetaParameterNameSurface:     "temperature",
				domain.MetaParameterOperation:       "set",
				domain.MetaParameterValueKind:       "number",
				domain.MetaParameterRawValue:        "0.2",
				domain.MetaParameterNormalizedValue: "0.2",
			},
		}}},
	}
	_, err := stage.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run err = nil, want split evidence span rejection")
	}
}

func TestCommitGraph_ParameterRejectsBareEvidenceRefs(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	state := &write.WriteState{
		Scope: scope,
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:      "param-1",
			Scope:   scope,
			Kind:    domain.KindParameter,
			Content: "experiment has parameter temperature set to 0.2.",
			EvidenceRefs: []domain.EvidenceRef{{
				ID:   "src-1",
				Text: "temperature 0.2",
			}},
		}}},
	}
	stage := stages.NewCommitGraph(&observationStoreRecorder{}, &linkStoreRecorder{}, nil)
	_, err := stage.Run(context.Background(), state)
	if err == nil {
		t.Fatal("Run err = nil, want graph_dependencies_missing")
	}
}

func TestCommitGraph_ParameterPreflightsCanonicalEvidenceRefs(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	cases := []struct {
		name   string
		store  *observationStoreRecorder
		spanID string
	}{
		{
			name:   "observation missing",
			store:  &observationStoreRecorder{},
			spanID: "span-1",
		},
		{
			name: "span missing",
			store: &observationStoreRecorder{getByID: map[string]domain.Observation{"obs-1": {
				ID:    "obs-1",
				Scope: scope,
				Spans: []domain.ObservationSpan{{
					ID:            "other-span",
					ObservationID: "obs-1",
				}},
			}}},
			spanID: "span-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &write.WriteState{
				Scope: scope,
				Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
					ID:      "param-1",
					Scope:   scope,
					Kind:    domain.KindParameter,
					Content: "experiment has parameter temperature set to 0.2.",
					EvidenceRefs: []domain.EvidenceRef{{
						ID:            "src-1",
						ObservationID: "obs-1",
						SpanID:        tc.spanID,
						Text:          "temperature = 0.2",
					}},
				}}},
			}
			stage := stages.NewCommitGraph(tc.store, &linkStoreRecorder{}, nil)
			_, err := stage.Run(context.Background(), state)
			if err == nil {
				t.Fatal("Run err = nil, want graph dependency preflight error")
			}
		})
	}
}

func TestCommitGraph_LinksNewFactToExistingAssertionsForSameObservation(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	existingObservation := domain.GraphNodeRef{Kind: domain.GraphNodeObservation, ID: "obs-1"}
	existingLink := graphledger.NewObservationFactLink(scope, domain.LinkSupports, "obs-1", "fact-existing", []domain.EvidenceRef{{
		ObservationID: "obs-1",
		SpanID:        "span-1",
		Text:          "temperature = 0.2",
	}}, time.Now())
	links := &linkStoreRecorder{byNode: map[string][]domain.FactLink{
		string(existingObservation.Kind) + "|" + existingObservation.ID: {existingLink},
	}}
	stage := stages.NewCommitGraph(&observationStoreRecorder{getByID: map[string]domain.Observation{
		"obs-1": {
			ID:    "obs-1",
			Scope: scope,
			Kind:  domain.ObservationKindTurn,
			Text:  "temperature = 0.2",
			Spans: []domain.ObservationSpan{{
				ID:            "span-1",
				ObservationID: "obs-1",
				Text:          "temperature = 0.2",
				Start:         0,
				End:           len("temperature = 0.2"),
			}},
		},
	}}, links, nil)
	state := &write.WriteState{
		Scope: scope,
		Resolution: domain.Resolution{Facts: []domain.TemporalFact{{
			ID:     "fact-new",
			Scope:  scope,
			Kind:   domain.KindParameter,
			Object: "0.2",
			EvidenceRefs: []domain.EvidenceRef{{
				ObservationID: "obs-1",
				SpanID:        "span-1",
				Text:          "temperature = 0.2",
			}},
			Metadata: map[string]any{
				domain.MetaParameterOwner:           "experiment",
				domain.MetaParameterCanonicalName:   "temperature",
				domain.MetaParameterNameSurface:     "temperature",
				domain.MetaParameterOperation:       "set",
				domain.MetaParameterValueKind:       "number",
				domain.MetaParameterRawValue:        "0.2",
				domain.MetaParameterNormalizedValue: "0.2",
			},
		}}},
	}
	if _, err := stage.Run(ctx, state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, link := range links.appended {
		if link.Type == domain.LinkSameObservation && link.From.ID == "fact-existing" && link.To.ID == "fact-new" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("appended links = %+v, want stable same_observation between existing and new assertions", links.appended)
	}
}

func TestCommitGraph_CompensateCleansProjection(t *testing.T) {
	observations := &observationStoreRecorder{}
	projection := &observationProjectionRecorder{}
	links := &linkStoreRecorder{}
	stage := stages.NewCommitGraph(observations, links, projection)
	state := &write.WriteState{
		Scope:               domain.Scope{RuntimeID: "rt", UserID: "u1"},
		GraphObservationIDs: []string{"obs-1"},
		GraphLinkIDs:        []string{"link-1"},
	}

	if err := stage.Compensate(context.Background(), state); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if len(projection.forgotten) != 1 || projection.forgotten[0] != "obs-1" {
		t.Fatalf("projection forgotten = %v, want obs-1", projection.forgotten)
	}
	if len(links.deleted) != 1 || links.deleted[0] != "link-1" {
		t.Fatalf("deleted links = %v, want link-1", links.deleted)
	}
	if len(observations.deleted) != 1 || observations.deleted[0] != "obs-1" {
		t.Fatalf("deleted observations = %v, want obs-1", observations.deleted)
	}
}
