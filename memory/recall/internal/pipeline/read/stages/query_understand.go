package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// QueryUnderstand runs the query compiler and populates state.Intent.
type QueryUnderstand struct {
	compiler port.IntentCompiler
}

// NewQueryUnderstand constructs a QueryUnderstand stage.
func NewQueryUnderstand(compiler port.IntentCompiler) *QueryUnderstand {
	return &QueryUnderstand{compiler: compiler}
}

// Name implements pipeline.Stage.
func (QueryUnderstand) Name() string { return "query_understand" }

// Run implements pipeline.Stage.
func (s *QueryUnderstand) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	compiled, err := s.compiler.Compile(ctx, port.IntentInput{
		Text:      state.Query.Text,
		Entities:  state.Query.Entities,
		Subject:   state.Query.Subject,
		Predicate: state.Query.Predicate,
		Object:    state.Query.Object,
		Kinds:     state.Query.Kinds,
		TimeRange: state.Query.TimeRange,
	})
	latency := time.Since(started)
	if err != nil {
		return diagnostic.QueryUnderstandDetail{
			QueryLen:   len(state.Query.Text),
			NERLatency: latency,
		}, err
	}
	limit := state.Query.Limit
	if limit <= 0 {
		limit = 10
	}
	intent := &domain.QueryIntent{
		Text:      compiled.Text,
		Entities:  compiled.Entities,
		Subject:   compiled.Subject,
		Predicate: compiled.Predicate,
		Object:    compiled.Object,
		Kinds:     append([]domain.FactKind(nil), compiled.Kinds...),
		TimeRange: compiled.TimeRange,
		Features:  compiled.Features,
		Scope:     state.Scope,
		Limit:     limit,
	}
	state.Intent = intent
	kinds := make([]string, len(intent.Kinds))
	for i, k := range intent.Kinds {
		kinds[i] = string(k)
	}
	return diagnostic.QueryUnderstandDetail{
		QueryLen:     len(intent.Text),
		Entities:     intent.Entities,
		Kinds:        kinds,
		Subject:      intent.Subject,
		HasTimeRange: !intent.TimeRange.IsZero(),
		NERLatency:   latency,
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*QueryUnderstand)(nil)
