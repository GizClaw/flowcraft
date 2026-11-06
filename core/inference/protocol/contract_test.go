package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
)

func request(t *testing.T) inference.GenerateRequest {
	t.Helper()
	req := inference.GenerateRequest{
		Model: model.ModelRef{
			ID:      model.ModelID{Provider: "fake", Name: "m1"},
			Profile: "default",
		},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hello"),
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("baseline request invalid: %v", err)
	}
	return req
}

// honoring is a compiler that consumes every active field: the shape a correct
// driver has. Specific tests narrow it to prove the contract check catches the
// difference.
func honoring(req inference.GenerateRequest, wire string) protocol.Compiled[string] {
	return protocol.Compiled[string]{Wire: wire, Fields: req.ActiveFields()}
}

func unaryResponse(text string) inference.GenerateResponse {
	return inference.GenerateResponse{
		Message:      message.NewTextMessage(message.RoleAssistant, text),
		FinishReason: inference.FinishStop,
	}
}

// TestContractRejectsDroppedField is the point of the whole kit: a compiler that
// silently ignores a requested option is a driver bug, not a quiet no-op.
func TestContractRejectsDroppedField(t *testing.T) {
	req := request(t)
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	req.Tools = tools

	compile := func(
		_ context.Context,
		r inference.GenerateRequest,
		_ protocol.Shape,
	) (protocol.Compiled[string], error) {
		honored := r.ActiveFields()
		kept := honored[:0]
		for _, field := range honored {
			if field != inference.FieldTools {
				kept = append(kept, field)
			}
		}
		return protocol.Compiled[string]{Wire: "wire", Fields: kept}, nil
	}
	driver, err := protocol.BindGenerate(
		"fake",
		compile,
		func(context.Context, inference.GenerateRequest, string) (string, error) { return "", nil },
		func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
			return unaryResponse("ok"), nil
		},
	)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	_, err = driver.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("dropped field accepted")
	}
	if !inference.IsKind(err, inference.CompilerContractViolation) {
		t.Fatalf("kind = %v, want CompilerContractViolation", err)
	}
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) || inferenceErr.Field != inference.FieldTools {
		t.Fatalf("violation does not name the field: %+v", inferenceErr)
	}
}

// TestContractAcceptsWarnedField is the other half: an option the provider
// cannot honor in full is fine as long as it is reported.
func TestContractAcceptsWarnedField(t *testing.T) {
	req := request(t)
	tools, err := inference.NewToolset([]message.ToolDefinition{{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	req.Tools = tools

	compile := func(
		_ context.Context,
		r inference.GenerateRequest,
		_ protocol.Shape,
	) (protocol.Compiled[string], error) {
		honored := r.ActiveFields()
		kept := honored[:0]
		for _, field := range honored {
			if field != inference.FieldTools {
				kept = append(kept, field)
			}
		}
		return protocol.Compiled[string]{
			Wire:   "wire",
			Fields: kept,
			Warnings: []inference.Warning{{
				Field:   inference.FieldTools,
				Code:    "not_supported",
				Message: "this model has no tool channel",
			}},
		}, nil
	}
	driver, err := protocol.BindGenerate(
		"fake",
		compile,
		func(context.Context, inference.GenerateRequest, string) (string, error) { return "", nil },
		func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
			return unaryResponse("ok"), nil
		},
	)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	response, err := driver.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("warned field rejected: %v", err)
	}
	if len(response.Warnings) != 1 || response.Warnings[0].Code != "not_supported" {
		t.Fatalf("warning was not carried to the response: %+v", response.Warnings)
	}
}

func TestContractRejectsInactiveField(t *testing.T) {
	compile := func(
		_ context.Context,
		r inference.GenerateRequest,
		_ protocol.Shape,
	) (protocol.Compiled[string], error) {
		fields := append(r.ActiveFields(), inference.FieldTools)
		return protocol.Compiled[string]{Wire: "wire", Fields: fields}, nil
	}
	driver, err := protocol.BindGenerate(
		"fake",
		compile,
		func(context.Context, inference.GenerateRequest, string) (string, error) { return "", nil },
		func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
			return unaryResponse("ok"), nil
		},
	)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	_, err = driver.Execute(context.Background(), request(t))
	if err == nil {
		t.Fatal("inactive field accepted")
	}
	if !inference.IsKind(err, inference.CompilerContractViolation) {
		t.Fatalf("kind = %v, want CompilerContractViolation", err)
	}
}

func TestUnaryPathJoinsWarningsAndValidatesResponse(t *testing.T) {
	compile := func(
		_ context.Context,
		r inference.GenerateRequest,
		_ protocol.Shape,
	) (protocol.Compiled[string], error) {
		compiled := honoring(r, "wire")
		compiled.Warnings = []inference.Warning{{
			Field:   inference.FieldOutput,
			Code:    "quantized",
			Message: "output folded",
		}}
		return compiled, nil
	}
	driver, err := protocol.BindGenerate(
		"fake",
		compile,
		func(_ context.Context, _ inference.GenerateRequest, wire string) (string, error) {
			if wire != "wire" {
				t.Fatalf("transport saw %q", wire)
			}
			return "raw", nil
		},
		func(_ context.Context, _ inference.GenerateRequest, raw string) (inference.GenerateResponse, error) {
			if raw != "raw" {
				t.Fatalf("decoder saw %q", raw)
			}
			response := unaryResponse("answer")
			response.Warnings = []inference.Warning{{
				Field:   inference.FieldOutput,
				Code:    "decoder",
				Message: "from decoder",
			}}
			return response, nil
		},
	)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	response, err := driver.Execute(context.Background(), request(t))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if response.Message.Content.Text() != "answer" {
		t.Fatalf("response = %q", response.Message.Content.Text())
	}
	if len(response.Warnings) != 2 ||
		response.Warnings[0].Code != "quantized" ||
		response.Warnings[1].Code != "decoder" {
		t.Fatalf("warnings not joined in compiler-first order: %+v", response.Warnings)
	}
}

func TestUnaryPathClassifiesFailures(t *testing.T) {
	okCompile := func(
		_ context.Context,
		r inference.GenerateRequest,
		_ protocol.Shape,
	) (protocol.Compiled[string], error) {
		return honoring(r, "wire"), nil
	}
	okDecode := func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
		return unaryResponse("ok"), nil
	}

	t.Run("transport failure is a provider failure", func(t *testing.T) {
		driver, err := protocol.BindGenerate(
			"fake",
			okCompile,
			func(context.Context, inference.GenerateRequest, string) (string, error) {
				return "", errors.New("boom")
			},
			okDecode,
		)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		_, err = driver.Execute(context.Background(), request(t))
		if !inference.IsKind(err, inference.ProviderFailure) {
			t.Fatalf("kind = %v, want ProviderFailure", err)
		}
	})

	t.Run("cancellation is an interruption", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		driver, err := protocol.BindGenerate(
			"fake",
			okCompile,
			func(context.Context, inference.GenerateRequest, string) (string, error) { return "", context.Canceled },
			okDecode,
		)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		_, err = driver.Execute(ctx, request(t))
		if !inference.IsKind(err, inference.OperationInterrupted) {
			t.Fatalf("kind = %v, want OperationInterrupted", err)
		}
	})

	t.Run("decode failure is a malformed response", func(t *testing.T) {
		driver, err := protocol.BindGenerate(
			"fake",
			okCompile,
			func(context.Context, inference.GenerateRequest, string) (string, error) { return "raw", nil },
			func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
				return inference.GenerateResponse{}, errors.New("garbage")
			},
		)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		_, err = driver.Execute(context.Background(), request(t))
		if !inference.IsKind(err, inference.InvalidProviderResponse) {
			t.Fatalf("kind = %v, want InvalidProviderResponse", err)
		}
	})

	t.Run("unclassified compiler failure is a driver bug", func(t *testing.T) {
		driver, err := protocol.BindGenerate(
			"fake",
			func(context.Context, inference.GenerateRequest, protocol.Shape) (protocol.Compiled[string], error) {
				return protocol.Compiled[string]{}, errors.New("nil map")
			},
			func(context.Context, inference.GenerateRequest, string) (string, error) { return "raw", nil },
			okDecode,
		)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		_, err = driver.Execute(context.Background(), request(t))
		if !inference.IsKind(err, inference.CompilerContractViolation) {
			t.Fatalf("kind = %v, want CompilerContractViolation", err)
		}
	})
}

func TestInvalidRequestNeverReachesTheCompiler(t *testing.T) {
	called := false
	driver, err := protocol.BindGenerate("fake",
		func(context.Context, inference.GenerateRequest, protocol.Shape) (protocol.Compiled[string], error) {
			called = true
			return protocol.Compiled[string]{Wire: "wire"}, nil
		},
		func(context.Context, inference.GenerateRequest, string) (string, error) { return "raw", nil },
		func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
			return unaryResponse("ok"), nil
		},
	)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	_, err = driver.Execute(context.Background(), inference.GenerateRequest{})
	if err == nil {
		t.Fatal("invalid request accepted")
	}
	if !inference.IsKind(err, inference.InvalidRequest) {
		t.Fatalf("kind = %v, want InvalidRequest", err)
	}
	if called {
		t.Fatal("compiler ran for an invalid request")
	}
}

func TestBindRequiresEveryStage(t *testing.T) {
	compile := func(context.Context, inference.GenerateRequest, protocol.Shape) (protocol.Compiled[string], error) {
		return protocol.Compiled[string]{}, nil
	}
	transport := func(context.Context, inference.GenerateRequest, string) (string, error) { return "", nil }
	decode := func(context.Context, inference.GenerateRequest, string) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{}, nil
	}
	if _, err := protocol.BindGenerate("fake", nil, transport, decode); err == nil {
		t.Fatal("nil compiler accepted")
	}
	if _, err := protocol.BindGenerate("fake", compile, nil, decode); err == nil {
		t.Fatal("nil transport accepted")
	}
	if _, err := protocol.BindGenerate("fake", compile, transport, nil); err == nil {
		t.Fatal("nil decoder accepted")
	}
	if _, err := protocol.BindGenerateStream[string, string]("fake", nil, nil, nil); err == nil {
		t.Fatal("nil streaming pipeline accepted")
	}
}

// fakeStream replays a fixed list of chunks.
type fakeStream struct {
	chunks []string
	index  int
	closed bool
}

func (s *fakeStream) Next(context.Context) (string, error) {
	if s.index >= len(s.chunks) {
		return "", io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

func streamDriver(t *testing.T, chunks ...string) (protocol.GenerateStreamDriver, *fakeStream) {
	t.Helper()
	raw := &fakeStream{chunks: chunks}
	driver, err := protocol.BindGenerateStream(
		"fake",
		func(_ context.Context, r inference.GenerateRequest, _ protocol.Shape) (protocol.Compiled[string], error) {
			return protocol.Compiled[string]{Wire: "wire", Fields: r.ActiveFields()}, nil
		},
		func(context.Context, inference.GenerateRequest, string) (protocol.RawStream[string], error) {
			return raw, nil
		},
		func(inference.GenerateRequest) protocol.StreamDecoder[string] {
			return func(_ context.Context, chunk string) ([]inference.StreamEvent, error) {
				if chunk == "end" {
					return []inference.StreamEvent{{Kind: inference.StreamEnd, Finish: inference.FinishStop}}, nil
				}
				return []inference.StreamEvent{{Kind: inference.StreamText, Text: chunk}}, nil
			}
		},
	)
	if err != nil {
		t.Fatalf("bind stream: %v", err)
	}
	return driver, raw
}

func TestStreamRelaysEventsAndAssemblesResult(t *testing.T) {
	driver, raw := streamDriver(t, "he", "llo", "end")
	// The fake compiler honors nothing, so the request must be empty of
	// optional fields; the baseline request only sets input fields.
	req := request(t)
	stream, err := driver.Open(context.Background(), req)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	var text strings.Builder
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event.Kind == inference.StreamText {
			text.WriteString(event.Text)
		}
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q", text.String())
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if response.Message.Content.Text() != "hello" {
		t.Fatalf("assembled = %q", response.Message.Content.Text())
	}
	if response.FinishReason != inference.FinishStop {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !raw.closed {
		t.Fatal("close did not reach the wire stream")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestStreamWithoutTerminalEventIsTruncation(t *testing.T) {
	driver, _ := streamDriver(t, "he", "llo")
	stream, err := driver.Open(context.Background(), request(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()
	for {
		_, err := stream.Next(context.Background())
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("truncated stream reported a clean end")
		}
		if !inference.IsKind(err, inference.ProviderTruncated) {
			t.Fatalf("kind = %v, want ProviderTruncated", err)
		}
		break
	}
	if _, err := stream.Result(); err == nil {
		t.Fatal("result of a truncated stream returned without error")
	}
}

func TestBindProviderReportsMissingHalf(t *testing.T) {
	provider := protocol.BindGenerator(nil, nil)
	if _, err := provider.Generate(context.Background(), request(t)); !inference.IsKind(err, inference.UnsupportedOperation) {
		t.Fatalf("kind = %v, want UnsupportedOperation", err)
	}
	if _, err := provider.GenerateStream(context.Background(), request(t)); !inference.IsKind(err, inference.UnsupportedOperation) {
		t.Fatalf("kind = %v, want UnsupportedOperation", err)
	}
}

type fakeCatalog struct{}

func (fakeCatalog) Models() []model.ModelDescriptor {
	return []model.ModelDescriptor{{
		ID: model.ModelID{Provider: "fake", Name: "m1"},
	}}
}

func (fakeCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	if id.Name == "m1" {
		return model.ModelDescriptor{ID: id}, true
	}
	return model.ModelDescriptor{}, false
}
