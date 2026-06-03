package intent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

func TestSemanticRouter_RoutesByEmbeddingExamples(t *testing.T) {
	router := NewSemanticRouter(testRouteEmbedder{}, WithThreshold(0.2))
	out, err := router.Route(context.Background(), port.IntentRouterInput{
		Text: "How many times did Melanie go to the beach?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Route.Strategy != domain.RecallStrategyCount {
		t.Fatalf("strategy = %q, want count (route=%+v)", out.Route.Strategy, out.Route)
	}
	if out.Route.Confidence <= 0 {
		t.Fatalf("confidence should be populated: %+v", out.Route)
	}
}

func TestSemanticRouter_FallsBackToSingleEmbeddingsWhenBatchUnavailable(t *testing.T) {
	router := NewSemanticRouter(batchUnavailableRouteEmbedder{}, WithThreshold(0.2))
	out, err := router.Route(context.Background(), port.IntentRouterInput{
		Text: "How many times did the person go to the beach?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Route.Strategy != domain.RecallStrategyCount {
		t.Fatalf("strategy = %q, want count (route=%+v)", out.Route.Strategy, out.Route)
	}
	if out.Route.FallbackReason != "" {
		t.Fatalf("unexpected route fallback: %+v", out.Route)
	}
}

func TestSemanticRouter_FallsBackWhenConfidenceLow(t *testing.T) {
	router := NewSemanticRouter(testRouteEmbedder{}, WithThreshold(0.99))
	out, err := router.Route(context.Background(), port.IntentRouterInput{
		Text: "unrelated opaque request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Route.Strategy != domain.RecallStrategyDefault || out.Route.FallbackReason != "low_confidence" {
		t.Fatalf("route = %+v, want low-confidence default", out.Route)
	}
}

func TestSemanticRouter_PreservesStructuredHints(t *testing.T) {
	out, err := NewSemanticRouter(testRouteEmbedder{}).Route(context.Background(), port.IntentRouterInput{
		Text:      "hello",
		Subject:   "alice",
		Predicate: "city",
		Object:    "paris",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "alice" || out.Predicate != "city" || out.Object != "paris" {
		t.Fatalf("structured hints not preserved: %+v", out)
	}
}

func TestSemanticRouter_PreservesLiteralFeaturesWithoutSemanticCueFlags(t *testing.T) {
	out, err := NewSemanticRouter(testRouteEmbedder{}).Route(context.Background(), port.IntentRouterInput{
		Text: "When did Avery read \"Becoming Nicole\"?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Features.HasTimeSignal() {
		t.Fatalf("literal features must not hard-code temporal question semantics: %+v", out.Features.Temporal)
	}
	if len(out.Features.Quoted) == 0 {
		t.Fatalf("quoted literal should be preserved: %+v", out.Features)
	}
	if !slices.Contains(out.Entities, "avery") {
		t.Fatalf("literal entity spans should be preserved as retrieval hints: %v", out.Entities)
	}
}

func TestSemanticRouter_ExtractsExplicitTimeRange(t *testing.T) {
	out, err := NewSemanticRouter(testRouteEmbedder{}).Route(context.Background(), port.IntentRouterInput{
		Text: "What painting did Jordan show to Avery on October 13, 2023?",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantFrom := time.Date(2023, time.October, 13, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2023, time.October, 14, 0, 0, 0, 0, time.UTC)
	if !out.TimeRange.From.Equal(wantFrom) || !out.TimeRange.To.Equal(wantTo) {
		t.Fatalf("time range = %s..%s, want %s..%s", out.TimeRange.From, out.TimeRange.To, wantFrom, wantTo)
	}
	if !out.Features.Temporal.HasExplicitDate {
		t.Fatalf("features should carry explicit date: %+v", out.Features.Temporal)
	}
}

func TestSemanticRouter_PreservesExplicitTimeRange(t *testing.T) {
	explicit := domain.TimeRange{
		From: time.Date(2024, time.May, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, time.May, 2, 0, 0, 0, 0, time.UTC),
	}
	out, err := NewSemanticRouter(testRouteEmbedder{}).Route(context.Background(), port.IntentRouterInput{
		Text:      "What happened on October 13, 2023?",
		TimeRange: explicit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimeRange.From.Equal(explicit.From) || !out.TimeRange.To.Equal(explicit.To) {
		t.Fatalf("time range = %+v, want explicit %+v", out.TimeRange, explicit)
	}
}

func TestExtractEntitiesFromText_SkipsStopwords(t *testing.T) {
	got := extractEntitiesFromText("What is the capital of France?")
	if slices.Contains(got, "what") || slices.Contains(got, "the") {
		t.Fatalf("stopwords leaked: %v", got)
	}
	if !slices.Contains(got, "france") {
		t.Fatalf("want france in %v", got)
	}
}

type testRouteEmbedder struct{}

func (testRouteEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return testRouteVector(text), nil
}

func (e testRouteEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := e.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		out[i] = vec
	}
	return out, nil
}

type batchUnavailableRouteEmbedder struct {
	testRouteEmbedder
}

func (batchUnavailableRouteEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("batch unavailable")
}

func testRouteVector(text string) []float32 {
	text = strings.ToLower(text)
	vec := make([]float32, 9)
	add := func(i int, terms ...string) {
		for _, term := range terms {
			if strings.Contains(text, term) {
				vec[i]++
			}
		}
	}
	add(0, "time", "date", "when", "duration", "ordering")
	add(1, "set", "members", "options", "items", "list")
	add(2, "number", "count", "frequency", "age", "duration", "how many", "how long", "times")
	add(3, "connect", "bridge", "multiple events", "multiple assertions")
	add(4, "shared", "overlap", "multiple entities")
	add(5, "stable attributes", "preferences", "traits", "identity", "status")
	add(6, "verify", "refute", "yes", "no", "claim")
	add(7, "hypothetical", "counterfactual", "alternate premise", "prediction")
	add(8, "opaque")
	return vec
}
