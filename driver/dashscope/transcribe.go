package dashscope

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

const transcribePath = "/api/v1/services/audio/asr/transcription"

type transcribeWire struct {
	model   string
	fileURL string
}

type transcribeResult struct {
	Properties struct {
		OriginalDurationMS *int64 `json:"original_duration_in_milliseconds"`
	} `json:"properties"`
	Transcripts []struct {
		Text string `json:"text"`
	} `json:"transcripts"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func compileTranscribe(declared ModelSpec, name string) protocol.Compiler[inference.GenerateRequest, transcribeWire] {
	return func(_ context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[transcribeWire], error) {
		if request.Output.Kind() != inference.OutputText {
			return protocol.Compiled[transcribeWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldOutput, fmt.Errorf("dashscope: transcription publishes text"))
		}
		if shape == protocol.ShapeStream {
			return protocol.Compiled[transcribeWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldStream, fmt.Errorf("dashscope: file transcription is unary"))
		}
		var source media.AudioSource
		found := false
		for _, part := range request.Input.Content.Parts {
			value, ok := part.(message.AudioPart)
			if !ok {
				return protocol.Compiled[transcribeWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: transcription reads one audio part"))
			}
			source = value.Source
			found = true
		}
		if !found || source.Kind() != media.SourceURL {
			return protocol.Compiled[transcribeWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: transcription fetches its file by URL"))
		}
		return protocol.Compiled[transcribeWire]{Wire: transcribeWire{model: wireModel(name, declared), fileURL: source.URL()}, Fields: request.ActiveFields()}, nil
	}
}

func transcribeRequest(wire transcribeWire) map[string]any {
	return map[string]any{"model": wire.model, "input": map[string]any{"file_urls": []string{wire.fileURL}}, "parameters": map[string]any{}}
}

func transportTranscribe(client *mediaClient) protocol.Transport[inference.GenerateRequest, transcribeWire, transcribeRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, wire transcribeWire) (transcribeRaw, error) {
		taskID, requestID, err := createTask(ctx, client, transcribePath, transcribeRequest(wire), "file transcription")
		if err != nil {
			return transcribeRaw{}, err
		}
		query, err := awaitTask(ctx, client, client.pollInterval(), taskID, "file transcription")
		if err != nil {
			return transcribeRaw{}, err
		}
		link, err := transcriptionURL(query)
		if err != nil {
			return transcribeRaw{}, err
		}
		payload, err := client.getArtifact(ctx, link)
		if err != nil {
			return transcribeRaw{}, err
		}
		var result transcribeResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return transcribeRaw{}, fmt.Errorf("dashscope: transcription result: %w", err)
		}
		if result.Code != "" || result.Message != "" {
			return transcribeRaw{}, errorBody{Code: result.Code, Message: result.Message}.failure("file transcription", http.StatusOK)
		}
		return transcribeRaw{result: result, requestID: nonEmpty(query.RequestID, requestID)}, nil
	}
}

type transcribeRaw struct {
	result    transcribeResult
	requestID string
}

func decodeTranscribe(_ context.Context, request inference.GenerateRequest, raw transcribeRaw) (inference.GenerateResponse, error) {
	texts := make([]string, 0, len(raw.result.Transcripts))
	for _, transcript := range raw.result.Transcripts {
		if transcript.Text != "" {
			texts = append(texts, transcript.Text)
		}
	}
	if len(texts) == 0 {
		return inference.GenerateResponse{}, fmt.Errorf("dashscope: transcription returned no text")
	}
	return inference.GenerateResponse{Message: message.Message{Role: message.RoleAssistant, Content: message.NewTextContent(strings.Join(texts, "\n"))},
		FinishReason: inference.FinishStop, Metadata: inference.Metadata{Model: request.Model.ID, RequestID: raw.requestID}}, nil
}

func (d *driver) compileTranscribe(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[transcribeWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[transcribeWire]{}, err
	}
	return compileTranscribe(declared, request.Model.ID.Name)(ctx, request, shape)
}
