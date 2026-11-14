package dashscope

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

const ttsPath = generatePathMultimodal

type ttsWire struct {
	model    string
	text     string
	voice    string
	language string
}

type ttsEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Output    struct {
		Audio struct {
			Data string `json:"data"`
			URL  string `json:"url"`
			ID   string `json:"id"`
		} `json:"audio"`
	} `json:"output"`
	Usage struct {
		Characters *int64 `json:"characters"`
	} `json:"usage"`
}

type ttsRaw struct{ body ttsEnvelope }

func ttsOptions(extensions inference.Extensions) (*TTSOptions, error) {
	var found *TTSOptions
	for _, extension := range extensions {
		options, ok := extension.(*TTSOptions)
		if !ok {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: extension %s/%s is not understood", extension.ProviderID(), extension.ExtensionID()))
		}
		if found != nil {
			return nil, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: duplicate tts options"))
		}
		found = options
	}
	return found, nil
}

func compileTTS(declared ModelSpec, name string) protocol.Compiler[inference.GenerateRequest, ttsWire] {
	return func(_ context.Context, request inference.GenerateRequest, _ protocol.Shape) (protocol.Compiled[ttsWire], error) {
		if request.Output.Kind() != inference.OutputAudio {
			return protocol.Compiled[ttsWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldOutput, fmt.Errorf("dashscope: tts serves audio output"))
		}
		options, err := ttsOptions(request.Extensions)
		if err != nil {
			return protocol.Compiled[ttsWire]{}, err
		}
		if options == nil || options.Voice == "" {
			return protocol.Compiled[ttsWire]{}, inference.NewError(inference.InvalidExtension, inference.FieldExtensions, fmt.Errorf("dashscope: speech synthesis requires tts_options.voice"))
		}
		var text string
		for _, part := range request.Input.Content.Parts {
			value, ok := part.(message.TextPart)
			if !ok {
				return protocol.Compiled[ttsWire]{}, inference.NewError(inference.UnsupportedFeature, inference.FieldContent, fmt.Errorf("dashscope: speech speaks text only"))
			}
			text += value.Text
		}
		if text == "" {
			return protocol.Compiled[ttsWire]{}, inference.NewError(inference.InvalidRequest, inference.FieldContent, fmt.Errorf("dashscope: speech synthesis needs text"))
		}
		return protocol.Compiled[ttsWire]{Wire: ttsWire{model: wireModel(name, declared), text: text, voice: options.Voice, language: options.Language}, Fields: request.ActiveFields()}, nil
	}
}

// ttsBody is the speech request: the voice and text are required, and the language
// hint disappears when the caller names none.
type ttsBody struct {
	Model string       `json:"model"`
	Input ttsBodyInput `json:"input"`
}

type ttsBodyInput struct {
	Text     string `json:"text"`
	Voice    string `json:"voice"`
	Language string `json:"language_type,omitempty"`
}

func ttsRequest(wire ttsWire) ttsBody {
	return ttsBody{
		Model: wire.model,
		Input: ttsBodyInput{Text: wire.text, Voice: wire.voice, Language: wire.language},
	}
}

func transportTTS(client *mediaClient) protocol.Transport[inference.GenerateRequest, ttsWire, ttsRaw] {
	return func(ctx context.Context, _ inference.GenerateRequest, wire ttsWire) (ttsRaw, error) {
		var envelope ttsEnvelope
		if err := client.postJSON(ctx, ttsPath, false, ttsRequest(wire), &envelope); err != nil {
			return ttsRaw{}, err
		}
		if envelope.Code != "" {
			return ttsRaw{}, errorBody{Code: envelope.Code, Message: envelope.Message, RequestID: envelope.RequestID}.failure("speech synthesis", 200)
		}
		if envelope.Output.Audio.URL == "" && envelope.Output.Audio.Data == "" {
			return ttsRaw{}, fmt.Errorf("dashscope: speech synthesis returned no audio")
		}
		return ttsRaw{body: envelope}, nil
	}
}

func decodeTTS(_ context.Context, request inference.GenerateRequest, raw ttsRaw) (inference.GenerateResponse, error) {
	audio := raw.body.Output.Audio
	var source media.AudioSource
	if audio.URL != "" {
		value, err := media.NewAudioURL(audio.URL, "")
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		source = value
	} else {
		payload, err := base64.StdEncoding.DecodeString(audio.Data)
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		value, err := media.NewAudioBytes(payload, "audio/wav")
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		source = value
	}
	out := inference.GenerateResponse{
		Message:      message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{message.AudioPart{Source: source}}}},
		FinishReason: inference.FinishStop,
		Metadata:     inference.Metadata{Model: request.Model.ID, RequestID: raw.body.RequestID, ResponseID: audio.ID},
	}
	if raw.body.Usage.Characters != nil {
		out.Usage.Work = map[usage.WorkKind]int64{usage.WorkInputCharacters: *raw.body.Usage.Characters}
	}
	return out, nil
}

func (d *driver) compileTTS(ctx context.Context, request inference.GenerateRequest, shape protocol.Shape) (protocol.Compiled[ttsWire], error) {
	declared, err := d.spec(request.Model.ID)
	if err != nil {
		return protocol.Compiled[ttsWire]{}, err
	}
	return compileTTS(declared, request.Model.ID.Name)(ctx, request, shape)
}
