package inference

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

func validGenerateTextRequest() GenerateRequest {
	return GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: Content{Parts: []Part{TextPart{Text: "hello"}}},
				Intent:  Intent{Text: &TextIntent{}},
			},
		},
	}
}

func nativeGenerateCompile[Wire any](wire Wire) GenerateCompiler[Wire] {
	return func(
		_ context.Context,
		_ ModelRef,
		request GenerateRequest,
		shape GenerateExecutionShape,
	) (Compiled[Wire], error) {
		active := request.ActiveFieldsFor(shape)
		decisions := make([]Decision, 0, len(active))
		for _, field := range active {
			decisions = append(decisions, Decision{Field: field, Disposition: Native})
		}
		return Compiled[Wire]{
			Wire: wire,
			Report: CompileReport{
				Operation: OperationGenerate,
				Decisions: decisions,
			},
		}, nil
	}
}

func TestGenerateStreamAggregatesPartsAndFinishedState(t *testing.T) {
	type wire struct{}
	driver, err := BindGenerateStream(
		nativeGenerateCompile(wire{}),
		func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: []GenerateStreamEvent{
				{PartIndex: 0, Delta: &TextPartDelta{Text: "hello"}},
				{PartIndex: 0, Delta: TextPartDelta{Text: " world"}},
				{
					Usage:        &Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
					FinishReason: FinishCompleted,
				},
			}}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateStream: %v", err)
	}
	model := ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}}
	stream, err := driver.Stream(context.Background(), model, validGenerateTextRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, err := stream.Result(); !IsKind(err, InvalidProviderResponse) {
		t.Fatalf("partial Result error = %v, want InvalidProviderResponse", err)
	}
	first, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, ok := first.Delta.(TextPartDelta); !ok {
		t.Fatalf("first delta type = %T, want normalized TextPartDelta", first.Delta)
	}
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := response.Message.Content.Parts[0].(TextPart).Text; got != "hello world" {
		t.Fatalf("text = %q", got)
	}
	if response.FinishReason != FinishCompleted ||
		response.Metadata.Model != model.ID ||
		response.Metadata.Operation != OperationGenerate {
		t.Fatalf("response = %+v", response)
	}
}

func TestGenerateStreamRejectsNilProviderStream(t *testing.T) {
	type wire struct{}
	driver, err := BindGenerateStream(
		nativeGenerateCompile(wire{}),
		func(context.Context, wire) (ProviderStream[string], error) {
			var stream *generateStringStream
			return stream, nil
		},
		func(context.Context, string) (GenerateStreamEvent, error) {
			return GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateStream: %v", err)
	}
	_, err = driver.Stream(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
		validGenerateTextRequest(),
	)
	if !IsKind(err, InvalidProviderResponse) {
		t.Fatalf("Stream error = %v, want InvalidProviderResponse", err)
	}
}

func TestGenerateStreamRejectsEOFBeforeFinish(t *testing.T) {
	type wire struct{}
	driver, err := BindGenerateStream(
		nativeGenerateCompile(wire{}),
		func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: []GenerateStreamEvent{{
				PartIndex: 0,
				Delta:     TextPartDelta{Text: "partial"},
			}}}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateStream: %v", err)
	}
	stream, err := driver.Stream(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
		validGenerateTextRequest(),
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, _ = stream.Next(context.Background())
	_, err = stream.Next(context.Background())
	if !IsKind(err, InvalidProviderResponse) {
		t.Fatalf("terminal error = %v, want InvalidProviderResponse", err)
	}
}

func TestGenerateStreamRejectsInvalidEventBeforeExposure(t *testing.T) {
	tests := []struct {
		name  string
		event GenerateStreamEvent
	}{
		{
			name:  "invalid_usage",
			event: GenerateStreamEvent{Usage: &Usage{InputTokens: 1, TotalTokens: 2}},
		},
		{
			name:  "invalid_finish_reason",
			event: GenerateStreamEvent{FinishReason: FinishReason("provider-specific")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type wire struct{}
			driver, err := BindGenerateStream(
				nativeGenerateCompile(wire{}),
				func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
					return &generateEventStream{events: []GenerateStreamEvent{tt.event}}, nil
				},
				func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
					return event, nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := driver.Stream(
				context.Background(),
				ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
				validGenerateTextRequest(),
			)
			if err != nil {
				t.Fatal(err)
			}
			event, err := stream.Next(context.Background())
			if !IsKind(err, InvalidProviderResponse) {
				t.Fatalf("Next event/error = %+v/%v, want zero event and InvalidProviderResponse", event, err)
			}
			if event != (GenerateStreamEvent{}) {
				t.Fatalf("Next exposed invalid event: %+v", event)
			}
		})
	}
}

func TestGenerateStreamResultReturnsDeepClone(t *testing.T) {
	type wire struct{}
	driver, err := BindGenerateStream(
		nativeGenerateCompile(wire{}),
		func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: []GenerateStreamEvent{
				{PartIndex: 0, Delta: TextPartDelta{Text: "original"}},
				{FinishReason: FinishCompleted},
			}}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := driver.Stream(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
		validGenerateTextRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := stream.Next(context.Background()); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	first, err := stream.Result()
	if err != nil {
		t.Fatal(err)
	}
	first.Message.Content.Parts[0] = TextPart{Text: "mutated"}
	first.Metadata.Decisions[0].Reason = "mutated"

	second, err := stream.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Message.Content.Parts[0].(TextPart).Text; got != "original" {
		t.Fatalf("second Result text = %q, want original", got)
	}
	if second.Metadata.Decisions[0].Reason != "" {
		t.Fatalf("second Result shared metadata: %+v", second.Metadata.Decisions)
	}
}

func TestGenerateStreamAggregatesEveryPartDeltaKind(t *testing.T) {
	type wire struct{}
	format := media.AudioFormat{Encoding: media.AudioEncodingMP3}
	imageSource, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	driver, err := BindGenerateStream(
		nativeGenerateCompile(wire{}),
		func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
			return &generateEventStream{events: []GenerateStreamEvent{
				{PartIndex: 0, Delta: TextPartDelta{Text: "text"}},
				{
					PartIndex: 1,
					Delta: ToolCallDelta{
						ID: "call-1", Name: "lookup", ArgumentsFragment: `{"q":`,
					},
				},
				{
					PartIndex: 1,
					Delta:     ToolCallDelta{ArgumentsFragment: `"flowcraft"}`},
				},
				{
					PartIndex: 2,
					Delta: AudioPartDelta{
						Data: []byte("first"), Format: &format,
					},
				},
				{PartIndex: 2, Delta: AudioPartDelta{Data: []byte("second")}},
				{
					PartIndex: 3,
					Delta: ImagePartDelta{
						Part: ImagePart{Source: imageSource},
					},
				},
				{
					PartIndex: 4,
					Delta:     ReasoningDelta{Text: "first thought. "},
				},
				{
					PartIndex: 4,
					Delta: ReasoningDelta{
						Text:      "second thought",
						Signature: "sig-terminal",
					},
				},
				{FinishReason: FinishToolCalls},
			}}, nil
		},
		func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
			return event, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateStream: %v", err)
	}
	request := validGenerateTextRequest()
	request.Input.Content.Intent.Image = &ImageIntent{}
	request.Input.Content.Intent.Audio = &AudioIntent{
		Voice:  media.VoiceSpec{ID: "voice"},
		Format: format,
	}
	request.Input.Content.Intent.Text.Tools = []tool.Definition{{
		Name:        "lookup",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	stream, err := driver.Stream(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
		request,
	)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		_, err = stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(response.Message.Content.Parts) != 5 {
		t.Fatalf("parts = %+v", response.Message.Content.Parts)
	}
	call := response.Message.Content.Parts[1].(ToolCallPart).Call
	if string(call.Arguments) != `{"q":"flowcraft"}` {
		t.Fatalf("tool arguments = %s", call.Arguments)
	}
	audio := response.Message.Content.Parts[2].(AudioPart)
	if got := string(audio.Source.Bytes()); got != "firstsecond" {
		t.Fatalf("audio = %q", got)
	}
	reasoning := response.Message.Content.Parts[4].(ReasoningPart)
	if reasoning.Text != "first thought. second thought" ||
		reasoning.Signature != "sig-terminal" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
}

func TestGenerateStreamRejectsPartTypeChangeAndDeltaAfterFinish(t *testing.T) {
	tests := []struct {
		name   string
		events []GenerateStreamEvent
	}{
		{
			name: "part_type_change",
			events: []GenerateStreamEvent{
				{PartIndex: 0, Delta: TextPartDelta{Text: "text"}},
				{PartIndex: 0, Delta: ToolCallDelta{Name: "lookup"}},
			},
		},
		{
			name: "delta_after_finish",
			events: []GenerateStreamEvent{
				{PartIndex: 0, Delta: TextPartDelta{Text: "text"}},
				{FinishReason: FinishCompleted},
				{PartIndex: 0, Delta: TextPartDelta{Text: "late"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type wire struct{}
			driver, err := BindGenerateStream(
				nativeGenerateCompile(wire{}),
				func(context.Context, wire) (ProviderStream[GenerateStreamEvent], error) {
					return &generateEventStream{events: tt.events}, nil
				},
				func(_ context.Context, event GenerateStreamEvent) (GenerateStreamEvent, error) {
					return event, nil
				},
			)
			if err != nil {
				t.Fatalf("BindGenerateStream: %v", err)
			}
			stream, err := driver.Stream(
				context.Background(),
				ModelRef{ID: ModelID{Provider: "fake", Name: "generate"}},
				validGenerateTextRequest(),
			)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			for {
				_, err = stream.Next(context.Background())
				if err != nil {
					break
				}
			}
			if !IsKind(err, InvalidProviderResponse) {
				t.Fatalf("terminal error = %v, want InvalidProviderResponse", err)
			}
		})
	}
}

type generateEventStream struct {
	events []GenerateStreamEvent
	index  int
}

func (s *generateEventStream) Next(context.Context) (GenerateStreamEvent, error) {
	if s.index == len(s.events) {
		return GenerateStreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*generateEventStream) Close() error { return nil }

type generateStringStream struct{}

func (*generateStringStream) Next(context.Context) (string, error) { return "", io.EOF }
func (*generateStringStream) Close() error                         { return nil }
