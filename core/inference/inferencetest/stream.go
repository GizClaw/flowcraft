package inferencetest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// CallExpectation is one tool call the assembled answer must contain.
type CallExpectation struct {
	ID        string
	Name      string
	Arguments string
}

// PartExpectation is one content part the assembled answer must contain. Only the
// fields that are set are compared; BytesLen applies to media parts.
type PartExpectation struct {
	Kind     message.PartKind
	Text     string
	BytesLen int
}

// UsageExpectation is the accounting the assembled answer must report. Only the
// fields that are set are compared.
type UsageExpectation struct {
	Input  int64
	Output int64
	Total  int64
	Work   map[usage.WorkKind]int64
}

// StreamSample is one canned server-sent-event stream.
type StreamSample struct {
	// Name labels the subtest.
	Name string
	// Body is the raw SSE payload.
	Body string
	// WantKind, when set, expects the stream to fail with that error kind.
	WantKind inference.ErrorKind
	// WantKinds is the exact event sequence, when the order itself is the point.
	WantKinds []inference.StreamEventKind
	// WantText is the assembled answer's text.
	WantText string
	// WantCalls are the tool calls the assembled answer must contain, in order.
	WantCalls []CallExpectation
	// WantParts are the content parts the assembled answer must contain, in order.
	WantParts []PartExpectation
	// WantUsage is the accounting the assembled answer must report.
	WantUsage *UsageExpectation
	// Check asserts anything driver-specific that the declarative fields cannot.
	Check func(t *testing.T, response inference.GenerateResponse)
}

// Equivalence pairs a unary sample with a stream that must assemble into the same
// answer.
type Equivalence struct {
	// Unary is the unary fixture and the sample it serves.
	Unary       ProviderFixture
	UnarySample Sample
	// Stream is the stream that must assemble into the same answer.
	Stream StreamSample
	// Compare asserts the two responses agree. Drivers supply this because only
	// they know which parts of a response are provider-assigned identities.
	Compare func(t *testing.T, unary, streamed inference.GenerateResponse)
}

// StreamFixture describes how to build a streaming provider.
type StreamFixture struct {
	ProviderID   string
	NewGenerator func(t *testing.T, baseURL string) (inference.Provider, error)
	Request      func(t *testing.T) inference.GenerateRequest
	// Samples are clean streams; each must end with a terminal event.
	Samples []StreamSample
	// Truncated is a stream that stops before its terminal event.
	Truncated *StreamSample
	// Equivalence, when set, is the cross-path assertion.
	Equivalence *Equivalence
}

// RunStream runs the streaming suites: ordering, truncation, assembly and
// equivalence with the unary path.
func RunStream(t *testing.T, fixture StreamFixture) {
	t.Helper()
	if fixture.NewGenerator == nil || fixture.Request == nil {
		t.Fatalf("inferencetest: stream fixture needs NewProvider and Request")
	}

	t.Run("events end at a terminal event", func(t *testing.T) {
		for _, sample := range fixture.Samples {
			t.Run(sample.Name, func(t *testing.T) {
				provider, request := serveStream(t, fixture.NewGenerator, sample.Body, fixture.Request)
				stream, err := provider.GenerateStream(context.Background(), request)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				t.Cleanup(func() { _ = stream.Close() })

				kinds := drain(t, stream)
				assertTerminalLast(t, kinds)
				if len(sample.WantKinds) > 0 {
					assertKinds(t, kinds, sample.WantKinds)
				}
				// A finished stream stays finished: reading again is not a second
				// pass over the content.
				if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
					t.Fatalf("next after the end = %v, want io.EOF", err)
				}
				response, err := stream.Result()
				if err != nil {
					t.Fatalf("result: %v", err)
				}
				if err := response.Validate(); err != nil {
					t.Fatalf("assembled response invalid: %v", err)
				}
				assertExpectations(t, sample, response)
				// Result is a value, not a cursor: asking twice must agree.
				again, err := stream.Result()
				if err != nil {
					t.Fatalf("second result: %v", err)
				}
				if again.Message.Content.Text() != response.Message.Content.Text() ||
					again.FinishReason != response.FinishReason ||
					again.Usage.TotalTokens() != response.Usage.TotalTokens() {
					t.Fatalf("result is not idempotent: %+v vs %+v", response, again)
				}
				if sample.Check != nil {
					sample.Check(t, response)
				}
			})
		}
	})

	t.Run("close is idempotent", func(t *testing.T) {
		if len(fixture.Samples) == 0 {
			t.Skip("no sample to close")
		}
		provider, request := serveStream(
			t, fixture.NewGenerator, fixture.Samples[0].Body, fixture.Request,
		)
		stream, err := provider.GenerateStream(context.Background(), request)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("first close: %v", err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})

	if fixture.Truncated != nil {
		t.Run("a stream without a terminal event is truncated", func(t *testing.T) {
			sample := *fixture.Truncated
			provider, request := serveStream(t, fixture.NewGenerator, sample.Body, fixture.Request)
			stream, err := provider.GenerateStream(context.Background(), request)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = stream.Close() }()

			var last error
			for {
				_, err := stream.Next(context.Background())
				if err == nil {
					continue
				}
				last = err
				break
			}
			if errors.Is(last, io.EOF) {
				t.Fatal("truncated stream reported a clean end")
			}
			want := sample.WantKind
			if want == "" {
				want = inference.ProviderTruncated
			}
			if !inference.IsKind(last, want) {
				t.Fatalf("kind = %v, want %v", last, want)
			}
			if _, err := stream.Result(); err == nil {
				t.Fatal("result of a truncated stream returned without error")
			}
		})
	}

	if fixture.Equivalence != nil {
		t.Run("streaming assembles the unary answer", func(t *testing.T) {
			equivalence := fixture.Equivalence
			unaryProvider, unaryRequest := equivalence.Unary.serve(t, equivalence.UnarySample)
			unary, err := unaryProvider.Generate(context.Background(), unaryRequest)
			if err != nil {
				t.Fatalf("unary: %v", err)
			}

			streamProvider, streamRequest := serveStream(
				t, fixture.NewGenerator, equivalence.Stream.Body, fixture.Request,
			)
			stream, err := streamProvider.GenerateStream(context.Background(), streamRequest)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = stream.Close() }()
			drain(t, stream)
			streamed, err := stream.Result()
			if err != nil {
				t.Fatalf("result: %v", err)
			}
			equivalence.Compare(t, unary, streamed)
		})
	}
}

// drain reads every event and returns their kinds.
func drain(t *testing.T, stream inference.Stream) []inference.StreamEventKind {
	t.Helper()
	var kinds []inference.StreamEventKind
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return kinds
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		kinds = append(kinds, event.Kind)
	}
}

// assertKinds compares the observed event sequence with the declared one.
func assertKinds(t *testing.T, got, want []inference.StreamEventKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (sequence %v)", i, got[i], want[i], got)
		}
	}
}

// assertExpectations applies the declarative assertions a sample declared.
func assertExpectations(
	t *testing.T,
	sample StreamSample,
	response inference.GenerateResponse,
) {
	t.Helper()
	if sample.WantText != "" {
		if got := response.Message.Content.Text(); got != sample.WantText {
			t.Fatalf("text = %q, want %q", got, sample.WantText)
		}
	}
	if len(sample.WantCalls) > 0 {
		calls := response.Message.ToolCalls()
		if len(calls) != len(sample.WantCalls) {
			t.Fatalf("tool calls = %d, want %d", len(calls), len(sample.WantCalls))
		}
		for i, want := range sample.WantCalls {
			if calls[i].ID != want.ID || calls[i].Name != want.Name ||
				calls[i].Arguments.String() != want.Arguments {
				t.Fatalf("call %d = %+v, want %+v", i, calls[i], want)
			}
		}
	}
	if len(sample.WantParts) > 0 {
		parts := response.Message.Content.Parts
		if len(parts) != len(sample.WantParts) {
			t.Fatalf("parts = %d, want %d", len(parts), len(sample.WantParts))
		}
		for i, want := range sample.WantParts {
			kind, text, bytesLen := partFacts(parts[i])
			if kind != want.Kind {
				t.Fatalf("part %d kind = %q, want %q", i, kind, want.Kind)
			}
			if want.Text != "" && text != want.Text {
				t.Fatalf("part %d text = %q, want %q", i, text, want.Text)
			}
			if want.BytesLen != 0 && bytesLen != want.BytesLen {
				t.Fatalf("part %d bytes = %d, want %d", i, bytesLen, want.BytesLen)
			}
		}
	}
	if sample.WantUsage != nil {
		got := response.Usage
		want := *sample.WantUsage
		if got.InputTokens != want.Input || got.OutputTokens != want.Output ||
			got.TotalTokens() != want.Total {
			t.Fatalf("usage = %d/%d/%d, want %d/%d/%d",
				got.InputTokens, got.OutputTokens, got.TotalTokens(),
				want.Input, want.Output, want.Total)
		}
		for kind, wantValue := range want.Work {
			if gotValue := got.WorkValue(kind); gotValue != wantValue {
				t.Fatalf("%s = %d, want %d", kind, gotValue, wantValue)
			}
		}
	}
}

// partFacts reduces a part to the three things the suite can compare generically.
func partFacts(part message.Part) (message.PartKind, string, int) {
	switch typed := part.(type) {
	case message.TextPart:
		return message.PartText, typed.Text, 0
	case message.ReasoningPart:
		return message.PartReasoning, typed.Text, 0
	case message.ToolCallPart:
		return message.PartToolCall, typed.Call.Name, typed.Call.Arguments.Len()
	case message.ImagePart:
		return message.PartImage, "", len(typed.Source.Bytes())
	case message.AudioPart:
		return message.PartAudio, "", len(typed.Source.Bytes())
	default:
		return part.Kind(), "", 0
	}
}

// assertTerminalLast requires a terminal event, and allows accounting to follow
// it: providers report usage in a trailing chunk, and dropping it would leave
// every streamed call reporting zero tokens.
func assertTerminalLast(t *testing.T, kinds []inference.StreamEventKind) {
	t.Helper()
	end := -1
	for i, kind := range kinds {
		if kind == inference.StreamEnd {
			if end >= 0 {
				t.Fatalf("two terminal events at %d and %d", end, i)
			}
			end = i
		}
	}
	if end < 0 {
		t.Fatalf("stream carried no terminal event: %v", kinds)
	}
	for _, kind := range kinds[end+1:] {
		if kind != inference.StreamUsage {
			t.Fatalf("event %q followed the terminal event: %v", kind, kinds)
		}
	}
}

// serveStream starts an endpoint that answers with an SSE body.
func serveStream(
	t *testing.T,
	newProvider func(*testing.T, string) (inference.Provider, error),
	body string,
	request func(*testing.T) inference.GenerateRequest,
) (inference.Generator, inference.GenerateRequest) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	provider, err := newProvider(t, server.URL)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return provider, request(t)
}
