package openai

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	otellog "go.opentelemetry.io/otel/log"
)

// Transcription runs on the audio transcriptions endpoint, unary only: the
// API takes complete files, so there is no streaming session to drive. Only
// inline audio can be uploaded — URL sources have no native channel and are
// rejected instead of fetched behind the caller's back. Segment timestamps
// switch the response to verbose_json; without them the plain json shape
// carries text and usage only.

type sttWire struct {
	model      string
	data       []byte
	filename   string
	language   string
	prompt     string
	timestamps bool
}

type sttRaw struct {
	text           string
	language       string // verbose_json only
	requestedLang  string // echoed from the wire for language attribution
	durationMillis int64  // verbose_json only
	segments       []sttSegment
	usage          sttUsage
}

type sttSegment struct {
	text        string
	startMillis int64
	endMillis   int64
}

type sttUsage struct {
	inputTokens    int64
	outputTokens   int64
	durationMillis int64
}

func compileTranscription(
	model string,
) inference.Compiler[inference.TranscriptionRequest, sttWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.TranscriptionRequest,
	) (inference.Compiled[sttWire], error) {
		ledger := newLedger(inference.OperationTranscription, request.ActiveFields())
		wire := sttWire{
			model:    model,
			language: request.Language,
			prompt:   request.Prompt,
		}
		switch request.Audio.Kind() {
		case media.SourceInline:
			wire.data = request.Audio.Bytes()
			wire.filename = audioFilename(request.Audio.MediaType())
		default:
			ledger.reject(
				inference.FieldTranscriptionAudio,
				"transcription uploads inline audio only; URL sources have no channel",
			)
		}
		if request.Timestamps != nil {
			wire.timestamps = *request.Timestamps
		}
		for _, field := range request.Extensions.ActiveFields() {
			ledger.reject(field, "openai transcription supports no extensions")
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[sttWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[sttWire]{Wire: wire, Report: report}, nil
	}
}

// namedReader gives the SDK's multipart encoder a truthful filename: the
// endpoint routes format detection on the file extension.
type namedReader struct {
	*bytes.Reader
	name string
}

func (r namedReader) Name() string { return r.name }

// audioFilename derives an upload filename from the source media type.
func audioFilename(mediaType string) string {
	switch mediaType {
	case "audio/mpeg":
		return "audio.mp3"
	case "audio/wav", "audio/x-wav":
		return "audio.wav"
	case "audio/opus", "audio/ogg":
		return "audio.ogg"
	case "audio/aac":
		return "audio.aac"
	case "audio/flac":
		return "audio.flac"
	case "audio/webm":
		return "audio.webm"
	case "audio/mp4", "audio/m4a":
		return "audio.m4a"
	default:
		return "audio"
	}
}

func transportTranscription(
	client openai.Client,
) inference.Transport[sttWire, sttRaw] {
	return func(ctx context.Context, wire sttWire) (sttRaw, error) {
		params := openai.AudioTranscriptionNewParams{
			File:  namedReader{Reader: bytes.NewReader(wire.data), name: wire.filename},
			Model: openai.AudioModel(wire.model),
		}
		if wire.language != "" {
			params.Language = param.NewOpt(wire.language)
		}
		if wire.prompt != "" {
			params.Prompt = param.NewOpt(wire.prompt)
		}
		if wire.timestamps {
			params.ResponseFormat = openai.AudioResponseFormatVerboseJSON
			params.TimestampGranularities = []string{"segment"}
		}
		response, err := client.Audio.Transcriptions.New(ctx, params)
		if err != nil {
			return sttRaw{}, classifyError(err)
		}
		raw := sttRaw{
			text:          response.Text,
			requestedLang: wire.language,
			usage: sttUsage{
				inputTokens:    response.Usage.InputTokens,
				outputTokens:   response.Usage.OutputTokens,
				durationMillis: int64(response.Usage.Seconds * 1000),
			},
		}
		if wire.timestamps {
			// verbose_json fields sit outside the SDK's typed struct. Extras
			// are tracked by map presence: respjson marks them unset even
			// though their raw payload is present.
			extra := response.JSON.ExtraFields
			if field, ok := extra["language"]; ok {
				// Language defaults to the SDK's empty value on parse
				// failure; surface the malformed payload so a server-
				// side change to the verbose_json shape is visible.
				if err := json.Unmarshal([]byte(field.Raw()), &raw.language); err != nil {
					telemetry.WarnErr(ctx, "openai stt: parse verbose_json.language", err,
						otellog.String("provider", "openai"),
						otellog.String("field", "language"))
				}
			}
			if field, ok := extra["duration"]; ok {
				var seconds float64
				if err := json.Unmarshal([]byte(field.Raw()), &seconds); err == nil {
					raw.durationMillis = int64(seconds * 1000)
				}
			}
			if field, ok := extra["segments"]; ok {
				raw.segments = parseSTTSegments(field.Raw())
			}
		}
		return raw, nil
	}
}

// parseSTTSegments lowers verbose_json segments to the provider-owned shape.
// Malformed payloads degrade to no segments rather than failing the decode:
// the transcript text remains truthful.
func parseSTTSegments(raw string) []sttSegment {
	var entries []struct {
		Text  string  `json:"text"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	segments := make([]sttSegment, 0, len(entries))
	for _, entry := range entries {
		segments = append(segments, sttSegment{
			text:        entry.Text,
			startMillis: int64(entry.Start * 1000),
			endMillis:   int64(entry.End * 1000),
		})
	}
	return segments
}

func decodeTranscription(
	_ context.Context,
	raw sttRaw,
) (inference.TranscriptionResponse, error) {
	response := inference.TranscriptionResponse{Text: raw.text}
	switch {
	case raw.requestedLang != "":
		response.Language = &inference.TranscriptLanguage{
			Code:   raw.requestedLang,
			Source: inference.TranscriptLanguageRequested,
		}
	case raw.language != "":
		response.Language = &inference.TranscriptLanguage{
			Code:   raw.language,
			Source: inference.TranscriptLanguageDetected,
		}
	}
	for _, segment := range raw.segments {
		response.Segments = append(response.Segments, inference.TranscriptSegment{
			Text:        segment.text,
			StartMillis: segment.startMillis,
			EndMillis:   segment.endMillis,
		})
	}
	response.Usage.AudioDurationMillis = raw.durationMillis
	if raw.usage.durationMillis > 0 && response.Usage.AudioDurationMillis == 0 {
		response.Usage.AudioDurationMillis = raw.usage.durationMillis
	}
	if raw.usage.inputTokens > 0 {
		input := raw.usage.inputTokens
		response.Usage.InputTokens = &input
	}
	if raw.usage.outputTokens > 0 {
		output := raw.usage.outputTokens
		response.Usage.OutputTokens = &output
	}
	return response, nil
}

func openTranscription(
	cls *clients,
	id inference.ModelID,
	_ string,
) (inference.TranscriptionOperations, error) {
	return KernelTranscription(cls.api, id.Name)
}
