package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Intent runs the query compiler and populates state.Intent.
type Intent struct {
	compiler port.IntentCompiler
}

// NewIntent constructs an Intent stage.
func NewIntent(compiler port.IntentCompiler) *Intent {
	return &Intent{compiler: compiler}
}

// Name implements pipeline.Stage.
func (Intent) Name() string { return "intent" }

// Run implements pipeline.Stage.
func (s *Intent) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
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
		return diagnostic.IntentDetail{
			RawQuery:   state.Query.Text,
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
		Scope:     state.Scope,
		Limit:     limit,
	}
	state.Intent = intent
	kinds := make([]string, len(intent.Kinds))
	for i, k := range intent.Kinds {
		kinds[i] = string(k)
	}
	return diagnostic.IntentDetail{
		RawQuery:     intent.Text,
		Entities:     intent.Entities,
		Kinds:        kinds,
		Subject:      intent.Subject,
		HasTimeRange: !intent.TimeRange.IsZero(),
		NERLatency:   latency,
	}, nil
}

var _ pipeline.Stage[*read.ReadState] = (*Intent)(nil)
