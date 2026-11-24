package route

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
)

// fakeStream replays a fixed event list, so a routing decision about a stream is
// observable without a provider.
type fakeStream struct {
	events []inference.StreamEvent
	next   int
	result inference.GenerateResponse
	closed bool
}

func (f *fakeStream) Next(context.Context) (inference.StreamEvent, error) {
	if f.next >= len(f.events) {
		return inference.StreamEvent{}, io.EOF
	}
	event := f.events[f.next]
	f.next++
	return event, nil
}

func (f *fakeStream) Result() (inference.GenerateResponse, error) { return f.result, nil }
func (f *fakeStream) Close() error                                { f.closed = true; return nil }

// streamingGenerator opens a stream, counts opens, and can fail the open.
type streamingGenerator struct {
	opens   int
	openErr error
	stream  *fakeStream
}

func (g *streamingGenerator) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, errors.New("unary not used")
}

func (g *streamingGenerator) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	g.opens++
	if g.openErr != nil {
		return nil, g.openErr
	}
	return g.stream, nil
}

func textEvent(text string) inference.StreamEvent {
	return inference.StreamEvent{Kind: inference.StreamText, Text: text}
}

// TestGenerateStreamFailsOverOnOpen pins the documented boundary: failover covers
// opening the stream and nothing after it, because replaying a stream that has
// already produced events would duplicate partial output.
func TestGenerateStreamFailsOverOnOpen(t *testing.T) {
	failing := &streamingGenerator{openErr: inference.NewError(
		inference.UnknownModel, inference.FieldModel, errors.New("not declared"))}
	stream := &fakeStream{events: []inference.StreamEvent{textEvent("from b")}}
	second := &streamingGenerator{stream: stream}

	router, err := New([]Target{
		{Name: "a", Provider: inference.Provider{Generator: failing}},
		{Name: "b", Provider: inference.Provider{Generator: second}},
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	trace := &Trace{}
	got, err := router.GenerateStream(WithTrace(context.Background(), trace), generationRequest())
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if got != stream {
		t.Fatal("GenerateStream returned a different stream")
	}
	if failing.opens != 1 || second.opens != 1 {
		t.Fatalf("opens = %d, %d; want one each", failing.opens, second.opens)
	}
	attempts := trace.Attempts()
	if len(attempts) != 2 || attempts[0].Target != "a" || attempts[1].Target != "b" {
		t.Fatalf("trace = %+v", attempts)
	}
	if attempts[0].Kind != inference.UnknownModel {
		t.Fatalf("first attempt kind = %q", attempts[0].Kind)
	}
}

// TestGenerateStreamDoesNotRetryARequestRejection keeps a caller's own mistake
// from being paid for once per target: the same request fails the same way
// everywhere.
func TestGenerateStreamDoesNotRetryARequestRejection(t *testing.T) {
	rejecting := &streamingGenerator{openErr: inference.NewError(
		inference.InvalidRequest, inference.FieldInput, errors.New("bad input"))}
	second := &streamingGenerator{stream: &fakeStream{}}

	router, err := New([]Target{
		{Name: "a", Provider: inference.Provider{Generator: rejecting}},
		{Name: "b", Provider: inference.Provider{Generator: second}},
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := router.GenerateStream(context.Background(), generationRequest()); err == nil {
		t.Fatal("GenerateStream = nil error, want the rejection")
	}
	if second.opens != 0 {
		t.Fatalf("second target opened %d streams after a request rejection", second.opens)
	}
}

// TestGenerateStreamHandsTheStreamOverVerbatim is the other half of the boundary:
// once open, the events are the provider's, in order, and routing does not touch
// them.
func TestGenerateStreamHandsTheStreamOverVerbatim(t *testing.T) {
	stream := &fakeStream{events: []inference.StreamEvent{textEvent("one"), textEvent("two")}}
	generator := &streamingGenerator{stream: stream}
	router, err := New([]Target{{Name: "a", Provider: inference.Provider{Generator: generator}}}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := router.GenerateStream(context.Background(), generationRequest())
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var seen []string
	for {
		event, err := got.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		seen = append(seen, event.Text)
	}
	if len(seen) != 2 || seen[0] != "one" || seen[1] != "two" {
		t.Fatalf("events = %v", seen)
	}
}

// TestDecideRoutesAndFailsOver covers the judgment seam, which shares the dispatch
// machinery with generation and therefore has to exhibit the same failover.
func TestDecideRoutesAndFailsOver(t *testing.T) {
	failing := &fakeDecider{reply: func(int) (inference.DecisionResponse, error) {
		return inference.DecisionResponse{}, inference.NewError(
			inference.UnknownModel, inference.FieldModel, errors.New("not declared"))
	}}
	second := &fakeDecider{reply: func(int) (inference.DecisionResponse, error) {
		probability := 0.5
		return inference.DecisionResponse{
			Answers: []inference.Answer{{
				Name: "qa", Kind: inference.AnswerKind(inference.QuestionPredicate), Probability: &probability,
			}},
		}, nil
	}}

	router, err := New([]Target{
		{Name: "a", Provider: inference.Provider{Decider: failing}},
		{Name: "b", Provider: inference.Provider{Decider: second}},
	}, Policy{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	trace := &Trace{}
	response, err := router.Decide(WithTrace(context.Background(), trace), decisionRequest(1))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(response.Answers) != 1 {
		t.Fatalf("response = %+v", response)
	}
	if failing.calls != 1 || second.calls != 1 {
		t.Fatalf("calls = %d, %d; want one each", failing.calls, second.calls)
	}
	if attempts := trace.Attempts(); len(attempts) != 2 || attempts[0].Kind != inference.UnknownModel {
		t.Fatalf("trace = %+v", attempts)
	}
}

// TestRouteCatalogMergesTargets keeps "which models can this router serve" honest:
// a host that shows a model list must see every target's line-up, and a lookup must
// answer with the target that actually declares it.
func TestRouteCatalogMergesTargets(t *testing.T) {
	first := routeCatalog{targets: []Target{{Name: "a", Provider: inference.Provider{
		Catalog: fakeCatalog{models: []model.ModelDescriptor{
			{ID: model.ModelID{Provider: "p", Name: "m1"}},
		}},
	}}}}
	merged := routeCatalog{targets: append(first.targets, Target{Name: "b", Provider: inference.Provider{
		Catalog: fakeCatalog{models: []model.ModelDescriptor{
			{ID: model.ModelID{Provider: "p", Name: "m2"}},
		}},
	}})}

	if got := len(merged.Models()); got != 2 {
		t.Fatalf("Models() returned %d entries, want 2", got)
	}
	if _, ok := merged.Describe(model.ModelID{Provider: "p", Name: "m2"}); !ok {
		t.Fatal("Describe missed a model the second target declares")
	}
	if _, ok := merged.Describe(model.ModelID{Provider: "p", Name: "absent"}); ok {
		t.Fatal("Describe invented a model nobody declares")
	}
}

type fakeCatalog struct {
	models []model.ModelDescriptor
}

func (c fakeCatalog) Models() []model.ModelDescriptor { return c.models }
func (c fakeCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}
