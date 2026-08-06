package bytedance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"

	doubaospeech "github.com/GizClaw/doubao-speech-go"
)

// Full-duplex dialogue runs on the Doubao realtime duplex WebSocket API. The
// service is audio-first: audio output is mandatory, text output is the
// spoken response's text channel, and video input has no native channel.
// Tool schemas pass through the provider's published JSON-schema subset;
// schemas using constructs outside that subset are rejected at compile time
// with the offending keyword named.

type realtimeWire struct {
	model        string
	instructions string
	audioOutput  bool
	textOutput   bool
	inputFormat  realtimeAudioFormat
	outputFormat realtimeAudioFormat
	voice        string
	tools        []realtimeToolWire
	// Extension settings (RealtimeOptions).
	outputSpeed    *int
	outputLoudness *int
}

type realtimeAudioFormat struct {
	typ  string // doubao audio type token
	rate int
}

type realtimeToolWire struct {
	name        string
	description string
	schema      *doubaospeech.RealtimeDuplexJSONSchema
}

type realtimeInputWire struct {
	kind   string // text | audio | tool_result
	text   string
	audio  []byte
	callID string
	output string
}

type realtimeRaw struct {
	kind     realtimeRawKind
	delta    string // text / transcript delta
	audio    []byte
	call     *realtimeRawCall
	usage    *rawUsage
	canceled bool
}

type realtimeRawKind int

const (
	realtimeRawText realtimeRawKind = iota
	realtimeRawAudio
	realtimeRawTranscript
	realtimeRawToolCall
	realtimeRawDone
)

type realtimeRawCall struct {
	id   string
	name string
	args []byte
}

func compileRealtime(
	cls *clients,
) inference.Compiler[inference.RealtimeConfig, realtimeWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		config inference.RealtimeConfig,
	) (inference.Compiled[realtimeWire], error) {
		ledger := newLedger(inference.OperationRealtime, config.ActiveFields())
		// The duplex "model" is a dialog engine version, not an Ark endpoint;
		// the profile's endpoints may pin a deployment-specific version,
		// otherwise the SDK default applies.
		modelVersion := doubaospeech.RealtimeDuplexModelDefault
		if endpoint, ok := cls.endpoints[model.ID.Name]; ok {
			modelVersion = endpoint
		}
		wire := realtimeWire{
			model:        modelVersion,
			instructions: config.Instructions,
		}

		for _, modality := range config.Modalities {
			switch modality {
			case inference.ModalityAudio:
				wire.audioOutput = true
			case inference.ModalityText:
				wire.textOutput = true
			}
		}
		if !wire.audioOutput {
			ledger.reject(
				inference.FieldRealtimeModalities,
				"duplex dialogue is audio-first; text-only output has no native mode",
			)
		}

		if format := config.InputAudioFormat; format != nil {
			wire.inputFormat = compileRealtimeFormat(
				*format,
				inference.FieldRealtimeInputAudioFormat,
				ledger,
			)
		} else {
			wire.inputFormat = realtimeAudioFormat{typ: "pcm", rate: 16000}
		}
		if format := config.OutputAudioFormat; format != nil {
			wire.outputFormat = compileRealtimeFormat(
				*format,
				inference.FieldRealtimeOutputAudioFormat,
				ledger,
			)
		}
		if voice := config.Voice; voice != nil {
			wire.voice = voice.ID
			if voice.Language != "" {
				// Duplex voices are fixed per ID; a requested language cannot
				// be negotiated and must not be silently dropped.
				ledger.reject(
					inference.FieldRealtimeVoice,
					"duplex voice language is fixed per voice ID",
				)
			}
		}
		for _, definition := range config.Tools {
			schema, err := realtimeToolSchema(definition.InputSchema)
			if err != nil {
				ledger.reject(inference.FieldRealtimeTools, err.Error())
				continue
			}
			wire.tools = append(wire.tools, realtimeToolWire{
				name:        definition.Name,
				description: definition.Description,
				schema:      schema,
			})
		}
		options, other := operationExtensions[RealtimeOptions](config.Extensions)
		rejectOtherExtensions("realtime dialogue", other, ledger)
		if options.OutputSpeed != nil {
			wire.outputSpeed = options.OutputSpeed
		}
		if options.OutputLoudness != nil {
			wire.outputLoudness = options.OutputLoudness
		}

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[realtimeWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[realtimeWire]{Wire: wire, Report: report}, nil
	}
}

// compileRealtimeFormat maps a canonical audio format to the duplex {type,
// rate} pair. Only 16-bit PCM has a truthful token.
func compileRealtimeFormat(
	format media.AudioFormat,
	field inference.FieldID,
	ledger *ledger,
) realtimeAudioFormat {
	if format.Encoding != media.AudioEncodingPCM16 {
		ledger.reject(
			field,
			fmt.Sprintf("duplex audio is 16-bit PCM, not %q", format.Encoding),
		)
		return realtimeAudioFormat{}
	}
	switch format.SampleRateHz {
	case 0:
		return realtimeAudioFormat{typ: "pcm", rate: 16000}
	case 8000, 16000, 24000:
		return realtimeAudioFormat{typ: "pcm", rate: format.SampleRateHz}
	default:
		ledger.reject(
			field,
			fmt.Sprintf("duplex accepts 8000, 16000, or 24000 Hz, not %d", format.SampleRateHz),
		)
		return realtimeAudioFormat{}
	}
}

// realtimeToolSchema converts a canonical JSON schema into the provider's
// published subset, rejecting anything outside it.
func realtimeToolSchema(
	raw json.RawMessage,
) (*doubaospeech.RealtimeDuplexJSONSchema, error) {
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("tool schema is not a JSON object: %w", err)
	}
	return convertSchemaNode(node)
}

var realtimeSchemaKeys = map[string]struct{}{
	"type":                 {},
	"description":          {},
	"properties":           {},
	"required":             {},
	"additionalProperties": {},
	"items":                {},
	"enum":                 {},
	"minLength":            {},
	"maxLength":            {},
	"minimum":              {},
	"maximum":              {},
	"anyOf":                {},
}

func convertSchemaNode(
	node map[string]any,
) (*doubaospeech.RealtimeDuplexJSONSchema, error) {
	for key := range node {
		if _, ok := realtimeSchemaKeys[key]; !ok {
			return nil, fmt.Errorf(
				"tool schema keyword %q is outside the provider's subset",
				key,
			)
		}
	}
	schema := &doubaospeech.RealtimeDuplexJSONSchema{}
	if typ, ok := node["type"].(string); ok {
		schema.Type = typ
	}
	if description, ok := node["description"].(string); ok {
		schema.Description = description
	}
	if required, ok := node["required"].([]any); ok {
		for _, entry := range required {
			if name, ok := entry.(string); ok {
				schema.Required = append(schema.Required, name)
			}
		}
	}
	if additional, ok := node["additionalProperties"].(bool); ok {
		schema.AdditionalProperties = &additional
	}
	if enum, ok := node["enum"].([]any); ok {
		for _, entry := range enum {
			if value, ok := entry.(string); ok {
				schema.Enum = append(schema.Enum, value)
			}
		}
	}
	schema.MinLength = intPointer(node["minLength"])
	schema.MaxLength = intPointer(node["maxLength"])
	schema.Minimum = floatPointer(node["minimum"])
	schema.Maximum = floatPointer(node["maximum"])
	if properties, ok := node["properties"].(map[string]any); ok {
		schema.Properties = make(map[string]*doubaospeech.RealtimeDuplexJSONSchema, len(properties))
		for name, value := range properties {
			child, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool schema property %q is not an object", name)
			}
			converted, err := convertSchemaNode(child)
			if err != nil {
				return nil, err
			}
			schema.Properties[name] = converted
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		converted, err := convertSchemaNode(items)
		if err != nil {
			return nil, err
		}
		schema.Items = converted
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		for _, value := range anyOf {
			child, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool schema anyOf entry is not an object")
			}
			converted, err := convertSchemaNode(child)
			if err != nil {
				return nil, err
			}
			schema.AnyOf = append(schema.AnyOf, converted)
		}
	}
	return schema, nil
}

func intPointer(value any) *int {
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	converted := int(number)
	return &converted
}

func floatPointer(value any) *float64 {
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	return &number
}

// ---------------------------------------------------------------------------
// Inputs.
// ---------------------------------------------------------------------------

func compileRealtimeInput(
	_ context.Context,
	_ inference.ModelRef,
	input inference.RealtimeInput,
) (inference.Compiled[realtimeInputWire], error) {
	active := input.ActiveFields()
	ledger := newLedger(inference.OperationRealtime, active)
	var wire realtimeInputWire
	switch value := input.(type) {
	case inference.RealtimeTextInput:
		wire = realtimeInputWire{kind: "text", text: value.Text}
	case inference.RealtimeAudioInput:
		wire = realtimeInputWire{kind: "audio", audio: bytesClone(value.Chunk.Data)}
	case inference.RealtimeVideoInput:
		ledger.reject(
			inference.FieldRealtimeInputVideo,
			"duplex dialogue has no video input channel",
		)
	case inference.RealtimeToolResultInput:
		wire = realtimeInputWire{
			kind:   "tool_result",
			callID: value.Result.CallID,
			output: value.Result.Content,
		}
	}
	report := ledger.report()
	if len(ledger.order) > 0 {
		return inference.Compiled[realtimeInputWire]{Report: report}, ledger.err()
	}
	return inference.Compiled[realtimeInputWire]{Wire: wire, Report: report}, nil
}

// ---------------------------------------------------------------------------
// Session.
// ---------------------------------------------------------------------------

func openRealtimeSession(
	client *doubaospeech.Client,
) inference.Transport[
	realtimeWire,
	inference.ProviderRealtimeSession[realtimeInputWire, realtimeRaw],
] {
	return func(
		ctx context.Context,
		wire realtimeWire,
	) (inference.ProviderRealtimeSession[realtimeInputWire, realtimeRaw], error) {
		config := &doubaospeech.RealtimeDuplexConfig{
			Session: doubaospeech.RealtimeDuplexSessionConfig{
				Model:        wire.model,
				Instructions: wire.instructions,
				Audio: doubaospeech.RealtimeDuplexAudioConfig{
					Input: doubaospeech.RealtimeDuplexAudioInputConfig{
						Format: doubaospeech.RealtimeDuplexAudioFormat{
							Type: wire.inputFormat.typ,
							Rate: wire.inputFormat.rate,
						},
					},
					Output: doubaospeech.RealtimeDuplexAudioOutputConfig{
						Format: doubaospeech.RealtimeDuplexAudioFormat{
							Type: wire.outputFormat.typ,
							Rate: wire.outputFormat.rate,
						},
						Voice: wire.voice,
					},
				},
			},
		}
		if wire.outputSpeed != nil {
			config.Session.Audio.Output.Speed = *wire.outputSpeed
		}
		if wire.outputLoudness != nil {
			config.Session.Audio.Output.Loudness = *wire.outputLoudness
		}
		for _, toolWire := range wire.tools {
			config.Session.Tools = append(
				config.Session.Tools,
				doubaospeech.RealtimeDuplexFunctionTool{
					Type:        "function",
					Name:        toolWire.name,
					Description: toolWire.description,
					Parameters:  toolWire.schema,
				},
			)
		}
		session, err := client.RealtimeDuplex.OpenSession(ctx, config)
		if err != nil {
			return nil, classifyError(err)
		}
		adapted := &realtimeSession{
			session: session,
			stop:    make(chan struct{}),
		}
		adapted.events = pumpRealtimeEvents(session, adapted.stop)
		return adapted, nil
	}
}

// realtimeSession adapts the SDK session to ProviderRealtimeSession. Lifecycle
// events (session.created, buffer commits, transcription completion) are
// protocol bookkeeping and never surface as canonical events.
type realtimeSession struct {
	session  *doubaospeech.RealtimeDuplexSession
	events   <-chan realtimeReceived
	stop     chan struct{}
	stopOnce sync.Once
}

type realtimeReceived struct {
	event *doubaospeech.RealtimeDuplexEvent
	err   error
}

func pumpRealtimeEvents(
	session *doubaospeech.RealtimeDuplexSession,
	stop <-chan struct{},
) <-chan realtimeReceived {
	events := make(chan realtimeReceived, 1)
	go func() {
		defer close(events)
		for {
			event, err := session.RecvEvent(context.Background())
			select {
			case events <- realtimeReceived{event: event, err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return events
}

func (s *realtimeSession) Send(
	ctx context.Context,
	wire realtimeInputWire,
) error {
	var err error
	switch wire.kind {
	case "text":
		err = s.session.SendSpeechText(ctx, doubaospeech.RealtimeDuplexSpeechTextRequest{
			Text: wire.text,
		})
	case "audio":
		err = s.session.SendAudio(ctx, wire.audio)
	case "tool_result":
		err = s.session.SendFunctionCallOutputs(
			ctx,
			doubaospeech.RealtimeDuplexFunctionCallOutput{
				CallID: wire.callID,
				Output: wire.output,
			},
		)
	default:
		err = fmt.Errorf("bytedance: unknown realtime input kind %q", wire.kind)
	}
	return classifyError(err)
}

func (s *realtimeSession) Next(ctx context.Context) (realtimeRaw, error) {
	if err := ctx.Err(); err != nil {
		return realtimeRaw{}, err
	}
	for {
		var received realtimeReceived
		var ok bool
		select {
		case <-ctx.Done():
			return realtimeRaw{}, ctx.Err()
		case received, ok = <-s.events:
		}
		if !ok {
			return realtimeRaw{}, io.EOF
		}
		if received.err != nil {
			return realtimeRaw{}, classifyError(received.err)
		}
		event := received.event
		if event == nil {
			continue
		}
		if failure := event.Error; failure != nil {
			return realtimeRaw{}, classifyError(failure)
		}
		switch event.Type {
		case doubaospeech.RealtimeDuplexEventResponseOutputTextDelta:
			if event.Delta == "" {
				continue
			}
			return realtimeRaw{kind: realtimeRawText, delta: event.Delta}, nil
		case doubaospeech.RealtimeDuplexEventResponseOutputAudioDelta:
			if len(event.Audio) == 0 {
				continue
			}
			return realtimeRaw{kind: realtimeRawAudio, audio: event.Audio}, nil
		case doubaospeech.RealtimeDuplexEventTranscriptionDelta:
			if event.Delta == "" {
				continue
			}
			return realtimeRaw{kind: realtimeRawTranscript, delta: event.Delta}, nil
		case doubaospeech.RealtimeDuplexEventResponseFunctionCallArgumentsDone:
			for _, call := range event.FunctionCalls {
				if call.CallID == "" || call.Name == "" {
					continue
				}
				return realtimeRaw{
					kind: realtimeRawToolCall,
					call: &realtimeRawCall{
						id:   call.CallID,
						name: call.Name,
						args: []byte(call.Arguments),
					},
				}, nil
			}
		case doubaospeech.RealtimeDuplexEventResponseDone:
			return realtimeRaw{
				kind:  realtimeRawDone,
				usage: realtimeUsage(event.Usage),
			}, nil
		case doubaospeech.RealtimeDuplexEventResponseCanceled:
			return realtimeRaw{kind: realtimeRawDone, canceled: true}, nil
		case doubaospeech.RealtimeDuplexEventError:
			return realtimeRaw{}, classifyError(
				fmt.Errorf("bytedance: realtime error event: %s", string(event.Raw)),
			)
		}
		// All other events are lifecycle bookkeeping: session.created,
		// session.updated, buffer commits, transcription started/completed,
		// conversation item CRUD, output started/done markers.
	}
}

func (s *realtimeSession) CancelResponse(ctx context.Context) error {
	return classifyError(s.session.CancelResponse(ctx))
}

func (s *realtimeSession) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	return classifyError(s.session.Close())
}

// realtimeUsage parses the provider's best-effort usage payload. The duplex
// usage object is loosely documented; unrecognized shapes degrade to nil
// rather than fabricating numbers.
func realtimeUsage(raw json.RawMessage) *rawUsage {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	usage := &rawUsage{
		inputTokens:  payload.InputTokens,
		outputTokens: payload.OutputTokens,
		totalTokens:  payload.TotalTokens,
	}
	if usage.totalTokens == 0 {
		usage.totalTokens = usage.inputTokens + usage.outputTokens
	}
	return usage
}

func decodeRealtimeEvent(
	_ context.Context,
	raw realtimeRaw,
) (inference.RealtimeEvent, error) {
	switch raw.kind {
	case realtimeRawText:
		return inference.RealtimeTextDeltaEvent{Delta: raw.delta}, nil
	case realtimeRawAudio:
		return inference.RealtimeAudioDeltaEvent{
			Chunk: media.AudioChunk{Data: bytesClone(raw.audio)},
		}, nil
	case realtimeRawTranscript:
		return inference.RealtimeTranscriptDeltaEvent{Delta: raw.delta}, nil
	case realtimeRawToolCall:
		if raw.call == nil {
			return nil, fmt.Errorf("bytedance: tool call event without payload")
		}
		arguments := json.RawMessage(raw.call.args)
		if !json.Valid(arguments) || len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		return inference.RealtimeToolCallEvent{Call: message.Call{
			ID:        raw.call.id,
			Name:      raw.call.name,
			Arguments: arguments,
		}}, nil
	case realtimeRawDone:
		finish := inference.FinishCompleted
		if raw.canceled {
			// A canceled response terminates without producing more output;
			// there is no more specific canonical finish reason.
			finish = inference.FinishOther
		}
		event := inference.RealtimeResponseDoneEvent{FinishReason: finish}
		if raw.usage != nil {
			event.Usage = rawUsageCanonical(*raw.usage)
		}
		return event, nil
	}
	return nil, fmt.Errorf("bytedance: unknown realtime raw kind %d", raw.kind)
}

func openRealtime(
	cls *clients,
	spec Spec,
	id inference.ModelID,
	profile string,
) (inference.RealtimeDriver, error) {
	speech, err := cls.requireSpeech(profile)
	if err != nil {
		return nil, err
	}
	return inference.BindRealtime(
		compileRealtime(cls),
		openRealtimeSession(speech),
		compileRealtimeInput,
		decodeRealtimeEvent,
	)
}
