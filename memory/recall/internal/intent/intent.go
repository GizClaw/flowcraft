package intent

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/sdk/embedding"
)

const defaultRouteThreshold = 0.42

type SemanticRouter struct {
	embedder  embedding.Embedder
	threshold float64

	once           sync.Once
	exampleVectors [][]float32
	exampleErr     error
}

type Option func(*SemanticRouter)

func WithThreshold(threshold float64) Option {
	return func(r *SemanticRouter) {
		if threshold > 0 {
			r.threshold = threshold
		}
	}
}

var _ port.IntentRouter = (*SemanticRouter)(nil)

func Default(embedder ...embedding.Embedder) port.IntentRouter {
	var emb embedding.Embedder
	if len(embedder) > 0 {
		emb = embedder[0]
	}
	return NewSemanticRouter(emb)
}

func NewSemanticRouter(embedder embedding.Embedder, opts ...Option) *SemanticRouter {
	r := &SemanticRouter{embedder: embedder, threshold: defaultRouteThreshold}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *SemanticRouter) Route(ctx context.Context, input port.IntentRouterInput) (port.IntentRouterResult, error) {
	features := ExtractFeatures(input.Text)
	entities := mergeEntities(input.Entities, extractEntitiesFromText(input.Text))
	route := r.route(ctx, input.Text)
	out := port.IntentRouterResult{
		Text:      input.Text,
		Subject:   input.Subject,
		Predicate: input.Predicate,
		Object:    input.Object,
		Kinds:     append([]domain.FactKind(nil), input.Kinds...),
		TimeRange: input.TimeRange,
		Entities:  entities,
		Features:  features,
		Route:     route,
	}
	if out.TimeRange.IsZero() {
		out.TimeRange = features.Temporal.TimeRange
	}
	return out, nil
}

func (r *SemanticRouter) route(ctx context.Context, text string) domain.IntentRoute {
	if strings.TrimSpace(text) == "" {
		return defaultRoute("empty_query")
	}
	if r == nil || r.embedder == nil {
		return defaultRoute("embedder_unavailable")
	}
	r.once.Do(func() {
		examples := routeExampleTexts()
		r.exampleVectors, r.exampleErr = r.embedder.EmbedBatch(ctx, examples)
	})
	if r.exampleErr != nil || len(r.exampleVectors) != len(defaultRouteExamples) {
		return defaultRoute("route_examples_unavailable")
	}
	queryVec, err := r.embedder.Embed(ctx, text)
	if err != nil || len(queryVec) == 0 {
		return defaultRoute("query_embedding_unavailable")
	}
	scores := routeScores(queryVec, r.exampleVectors)
	if len(scores) == 0 {
		return defaultRoute("no_route_scores")
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Confidence != scores[j].Confidence {
			return scores[i].Confidence > scores[j].Confidence
		}
		return scores[i].Strategy < scores[j].Strategy
	})
	best := scores[0]
	route := domain.IntentRoute{
		Strategy:   best.Strategy,
		Confidence: best.Confidence,
		Alternates: append([]domain.IntentRouteCandidate(nil), scores[1:min(4, len(scores))]...),
		Signals:    []string{"embedding_route"},
	}
	if best.Confidence < r.threshold {
		route.Strategy = domain.RecallStrategyDefault
		route.FallbackReason = "low_confidence"
	}
	return route
}

func defaultRoute(reason string) domain.IntentRoute {
	return domain.IntentRoute{
		Strategy:       domain.RecallStrategyDefault,
		FallbackReason: reason,
	}
}

type routeExample struct {
	strategy domain.RecallStrategy
	text     string
}

var defaultRouteExamples = []routeExample{
	{domain.RecallStrategyTemporal, "When did the event happen?"},
	{domain.RecallStrategyTemporal, "What date did the person attend the meeting?"},
	{domain.RecallStrategyTemporal, "When was the class or trip?"},
	{domain.RecallStrategySet, "What items did the person buy?"},
	{domain.RecallStrategySet, "Which books did the person read?"},
	{domain.RecallStrategySet, "What are the plans for summer?"},
	{domain.RecallStrategyCount, "How many times did this happen?"},
	{domain.RecallStrategyCount, "How many children does the person have?"},
	{domain.RecallStrategyCount, "How long has the person been doing this?"},
	{domain.RecallStrategyJoin, "What book did one person read from another person's suggestion?"},
	{domain.RecallStrategyJoin, "What did someone do after the earlier trip?"},
	{domain.RecallStrategyJoin, "Which fact connects two remembered events?"},
	{domain.RecallStrategyIntersection, "What subject did both people paint?"},
	{domain.RecallStrategyIntersection, "What do the two people have in common?"},
	{domain.RecallStrategyProfile, "What is the person like?"},
	{domain.RecallStrategyProfile, "What are the person's preferences or traits?"},
	{domain.RecallStrategyYesNo, "Would this statement be true?"},
	{domain.RecallStrategyYesNo, "Is the person a member of the group?"},
	{domain.RecallStrategyCounterfactual, "Would this still happen if something had not happened?"},
	{domain.RecallStrategyCounterfactual, "What would likely happen in a hypothetical situation?"},
}

func routeExampleTexts() []string {
	out := make([]string, len(defaultRouteExamples))
	for i, example := range defaultRouteExamples {
		out[i] = example.text
	}
	return out
}

func routeScores(query []float32, vectors [][]float32) []domain.IntentRouteCandidate {
	bestByStrategy := map[domain.RecallStrategy]float64{}
	for i, vec := range vectors {
		if i >= len(defaultRouteExamples) {
			break
		}
		score := cosine(query, vec)
		strategy := defaultRouteExamples[i].strategy
		if prev, ok := bestByStrategy[strategy]; !ok || score > prev {
			bestByStrategy[strategy] = score
		}
	}
	out := make([]domain.IntentRouteCandidate, 0, len(bestByStrategy))
	for strategy, score := range bestByStrategy {
		out = append(out, domain.IntentRouteCandidate{Strategy: strategy, Confidence: score})
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, an, bn float64
	for i := range a {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		an += av * av
		bn += bv * bv
	}
	if an == 0 || bn == 0 {
		return 0
	}
	return dot / (math.Sqrt(an) * math.Sqrt(bn))
}

func mergeEntities(explicit, extracted []string) []string {
	seen := make(map[string]struct{}, len(explicit)+len(extracted))
	add := func(s string) []string {
		s = words.NormalizeIntentEntityMention(s)
		if s == "" {
			return nil
		}
		if _, ok := seen[s]; ok {
			return nil
		}
		seen[s] = struct{}{}
		return []string{s}
	}
	var out []string
	for _, e := range explicit {
		out = append(out, add(e)...)
	}
	for _, e := range extracted {
		out = append(out, add(e)...)
	}
	return out
}

// extractEntitiesFromText is a conservative rule baseline: quoted spans,
// capitalized tokens, and CJK runs. Common question words are filtered
// (via recall/internal/words) so "Who did Alice meet in Paris?" yields alice
// and paris, not who.
func extractEntitiesFromText(text string) []string {
	return words.ExtractIntentEntityMentions(text)
}
