package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

type transcribeWire struct {
	url string
}

type transcribePayload struct {
	Text     string `json:"text"`
	Segments []struct {
		Text  string `json:"text"`
		Start int64  `json:"start"`
		End   int64  `json:"end"`
	} `json:"segments"`
}

// fakeTranscriber builds a real pipeline: the transport performs an HTTP call,
// so the conformance suite exercises the same stages a provider driver does.
func fakeTranscriber(baseURL string) (inference.Transcriber, error) {
	compile := func(_ context.Context, request inference.TranscribeRequest, shape protocol.Shape) (protocol.Compiled[transcribeWire], error) {
		if err := shape.Validate(); err != nil {
			return protocol.Compiled[transcribeWire]{}, err
		}
		return protocol.Compiled[transcribeWire]{
			Wire:   transcribeWire{url: baseURL + "/v1/audio/transcriptions"},
			Fields: request.ActiveFields(),
		}, nil
	}
	transport := func(ctx context.Context, _ inference.TranscribeRequest, wire transcribeWire) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, wire.url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, inference.NewError(inference.ProviderFailure, model.OperationTranscription, "", errors.New(resp.Status))
		}
		return body, nil
	}
	decode := func(_ context.Context, _ inference.TranscribeRequest, raw []byte) (inference.TranscribeResponse, error) {
		var payload transcribePayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return inference.TranscribeResponse{}, err
		}
		out := inference.TranscribeResponse{Text: payload.Text}
		for _, segment := range payload.Segments {
			out.Segments = append(out.Segments, inference.TranscriptSegment{Text: segment.Text})
		}
		return out, nil
	}
	return protocol.BindTranscriber("fake", compile, transport, decode)
}

func transcribeRequest(t *testing.T) inference.TranscribeRequest {
	t.Helper()
	source, err := media.NewAudioBytes([]byte("not really audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	return inference.TranscribeRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "fake", Name: "asr"}},
		Audio: message.AudioPart{Source: source},
	}
}

// TestRunTranscriber drives the conformance suite through a real HTTP endpoint.
func TestRunTranscriber(t *testing.T) {
	inferencetest.RunTranscriber(t, inferencetest.TranscriberFixture{
		ProviderID:     "fake",
		NewTranscriber: func(t *testing.T, baseURL string) (inference.Transcriber, error) { return fakeTranscriber(baseURL) },
		Request:        transcribeRequest,
		Samples: []inferencetest.TranscribeSample{
			{Name: "text", Body: `{"text":"hello"}`, WantText: "hello"},
			{Name: "provider failure", Status: http.StatusInternalServerError, WantKind: inference.ProviderFailure},
		},
	})
}

func TestBindTranscriberRejectsMissingStages(t *testing.T) {
	compile := func(context.Context, inference.TranscribeRequest, protocol.Shape) (protocol.Compiled[transcribeWire], error) {
		return protocol.Compiled[transcribeWire]{}, nil
	}
	transport := func(context.Context, inference.TranscribeRequest, transcribeWire) ([]byte, error) { return nil, nil }
	decode := func(context.Context, inference.TranscribeRequest, []byte) (inference.TranscribeResponse, error) {
		return inference.TranscribeResponse{}, nil
	}
	if _, err := protocol.BindTranscriber("", compile, transport, decode); err == nil {
		t.Error("empty provider id accepted")
	}
	if _, err := protocol.BindTranscriber("p", nil, transport, decode); err == nil {
		t.Error("nil compiler accepted")
	}
	if _, err := protocol.BindTranscriber("p", compile, nil, decode); err == nil {
		t.Error("nil transport accepted")
	}
	if _, err := protocol.BindTranscriber("p", compile, transport, nil); err == nil {
		t.Error("nil decoder accepted")
	}
}

// A compiler that drops an active field is a driver bug, and the shared ledger
// must reject it for transcription exactly as it does for generation.
func TestTranscriberContractCheck(t *testing.T) {
	compile := func(context.Context, inference.TranscribeRequest, protocol.Shape) (protocol.Compiled[transcribeWire], error) {
		return protocol.Compiled[transcribeWire]{Fields: []inference.Field{inference.FieldModel}}, nil
	}
	transport := func(context.Context, inference.TranscribeRequest, transcribeWire) ([]byte, error) { return nil, nil }
	decode := func(context.Context, inference.TranscribeRequest, []byte) (inference.TranscribeResponse, error) {
		return inference.TranscribeResponse{Text: "hi"}, nil
	}
	transcriber, err := protocol.BindTranscriber("fake", compile, transport, decode)
	if err != nil {
		t.Fatalf("BindTranscriber: %v", err)
	}
	_, err = transcriber.Transcribe(context.Background(), transcribeRequest(t))
	if !inference.IsKind(err, inference.CompilerContractViolation) {
		t.Errorf("Transcribe = %v, want CompilerContractViolation (the audio field was dropped)", err)
	}
}

type transcribeCatalog struct {
	models []model.ModelDescriptor
}

func (c transcribeCatalog) Models() []model.ModelDescriptor { return c.models }
func (c transcribeCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.models {
		if descriptor.ID == id {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}

func TestCheckCatalogTranscription(t *testing.T) {
	declaring := transcribeCatalog{models: []model.ModelDescriptor{{
		ID:         model.ModelID{Provider: "fake", Name: "asr"},
		Operations: []model.Operation{model.OperationTranscription},
	}}}
	generating := transcribeCatalog{models: []model.ModelDescriptor{{
		ID:         model.ModelID{Provider: "fake", Name: "chat"},
		Operations: []model.Operation{model.OperationGenerate},
	}}}

	if err := protocol.CheckCatalogTranscription(declaring, nil); err == nil {
		t.Error("a declared transcriptions with no transcriber was accepted")
	}
	transcriber, err := fakeTranscriber("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("fakeTranscriber: %v", err)
	}
	if err := protocol.CheckCatalogTranscription(declaring, transcriber); err != nil {
		t.Errorf("declared and bound rejected: %v", err)
	}
	if err := protocol.CheckCatalogTranscription(generating, nil); err != nil {
		t.Errorf("a catalog that declares no transcription rejected: %v", err)
	}
}
