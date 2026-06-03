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

const defaultRouteThreshold = 0.34

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
		r.exampleVectors, r.exampleErr = embedRouteExamples(ctx, r.embedder, examples)
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

func embedRouteExamples(ctx context.Context, emb embedding.Embedder, examples []string) ([][]float32, error) {
	vectors, err := emb.EmbedBatch(ctx, examples)
	if err == nil && len(vectors) == len(examples) {
		return vectors, nil
	}
	out := make([][]float32, len(examples))
	for i, example := range examples {
		vec, embedErr := emb.Embed(ctx, example)
		if embedErr != nil {
			if err != nil {
				return nil, err
			}
			return nil, embedErr
		}
		out[i] = vec
	}
	return out, nil
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
	{domain.RecallStrategyTemporal, "Question asking for the time or date of an event."},
	{domain.RecallStrategyTemporal, "Question asking when a planned or completed action occurs."},
	{domain.RecallStrategyTemporal, "Question requiring temporal ordering or duration."},
	{domain.RecallStrategySet, "Question asking for all members of a remembered set."},
	{domain.RecallStrategySet, "Question asking which options, items, entities, or activities were mentioned."},
	{domain.RecallStrategySet, "Question requiring complete list coverage rather than one match."},
	{domain.RecallStrategyCount, "Question asking for a number, count, frequency, age, or duration."},
	{domain.RecallStrategyCount, "Question requiring numeric aggregation over remembered facts."},
	{domain.RecallStrategyJoin, "Question requiring one remembered fact to connect to another remembered fact."},
	{domain.RecallStrategyJoin, "Question requiring a bridge across multiple events or assertions."},
	{domain.RecallStrategyIntersection, "Question asking what is shared by multiple entities or conditions."},
	{domain.RecallStrategyIntersection, "Question requiring the overlap between remembered sets or facts."},
	{domain.RecallStrategyProfile, "Question asking for stable attributes, preferences, identity, status, or relationships."},
	{domain.RecallStrategyProfile, "Question asking what is generally true about an entity."},
	{domain.RecallStrategyYesNo, "Question asking whether remembered facts verify or refute a claim."},
	{domain.RecallStrategyYesNo, "Question whose answer should start with yes or no when evidence is sufficient."},
	{domain.RecallStrategyCounterfactual, "Question asking about a hypothetical or counterfactual condition."},
	{domain.RecallStrategyCounterfactual, "Question requiring prediction under an alternate premise using remembered context."},
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
