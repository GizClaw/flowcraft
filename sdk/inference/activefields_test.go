package inference

import (
	"reflect"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// reflectActiveFields reproduces the reflection-based ledger scan that the
// hand-written ActiveFields implementations replaced. It exists only to pin
// the hand-written implementations to the same field ordering and zero-value
// semantics.
func reflectActiveFields(request any) []FieldID {
	value := reflect.ValueOf(request)
	requestType := value.Type()
	fields := make([]FieldID, 0, requestType.NumField())
	for i := 0; i < requestType.NumField(); i++ {
		tag := requestType.Field(i).Tag.Get("ledger")
		if tag == "" || tag == "extension" || !reflectFieldActive(value.Field(i)) {
			continue
		}
		fields = append(fields, FieldID(tag))
	}
	return fields
}

func reflectFieldActive(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Map, reflect.Slice, reflect.String:
		return value.Len() > 0
	case reflect.Interface, reflect.Pointer:
		return !value.IsNil()
	default:
		return !value.IsZero()
	}
}

func TestHandWrittenActiveFieldsMatchReflectiveScan(t *testing.T) {
	audio, err := media.NewAudioURL("https://example.com/audio.mp3", "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	dimensions := 256
	timestamps := true
	format := media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 24_000,
		Channels:     1,
	}
	voice := media.VoiceSpec{ID: "alloy", Language: "en"}
	tools := []message.Definition{{Name: "lookup"}}

	cases := []struct {
		name    string
		request any
	}{
		{name: "embed zero", request: EmbedRequest{}},
		{name: "embed items", request: EmbedRequest{
			Items: []EmbedItem{{Content: message.Content{
				Parts: []message.Part{message.TextPart{Text: "hello"}},
			}}},
		}},
		{name: "embed dimensions", request: EmbedRequest{Dimensions: &dimensions}},
		{name: "embed full with extension", request: EmbedRequest{
			Items:      []EmbedItem{{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hello"}}}}},
			Dimensions: &dimensions,
			Extensions: Extensions{testExtension{
				provider: "fake",
				id:       "embed_options",
				fields:   []ExtensionField{"service_tier"},
			}},
		}},

		{name: "transcription zero", request: TranscriptionRequest{}},
		{name: "transcription audio", request: TranscriptionRequest{Audio: audio}},
		{name: "transcription language", request: TranscriptionRequest{Language: "zh"}},
		{name: "transcription prompt", request: TranscriptionRequest{Prompt: "punctuate"}},
		{name: "transcription timestamps", request: TranscriptionRequest{Timestamps: &timestamps}},
		{name: "transcription full with extension", request: TranscriptionRequest{
			Audio:      audio,
			Language:   "zh",
			Prompt:     "punctuate",
			Timestamps: &timestamps,
			Extensions: Extensions{testExtension{
				provider: "fake",
				id:       "transcription_options",
				fields:   []ExtensionField{"language_hints"},
			}},
		}},

		{name: "session zero", request: TranscriptionSessionConfig{}},
		{name: "session input format", request: TranscriptionSessionConfig{InputFormat: format}},
		{name: "session language", request: TranscriptionSessionConfig{Language: "zh"}},
		{name: "session timestamps", request: TranscriptionSessionConfig{Timestamps: &timestamps}},
		{name: "session full", request: TranscriptionSessionConfig{
			InputFormat: format,
			Language:    "zh",
			Prompt:      "punctuate",
			Timestamps:  &timestamps,
		}},

		{name: "realtime zero", request: RealtimeConfig{}},
		{name: "realtime instructions", request: RealtimeConfig{Instructions: "be terse"}},
		{name: "realtime modalities", request: RealtimeConfig{Modalities: []Modality{ModalityText}}},
		{name: "realtime input format", request: RealtimeConfig{InputAudioFormat: &format}},
		{name: "realtime output format", request: RealtimeConfig{OutputAudioFormat: &format}},
		{name: "realtime voice", request: RealtimeConfig{Voice: &voice}},
		{name: "realtime tools", request: RealtimeConfig{Tools: tools}},
		{name: "realtime full with extension", request: RealtimeConfig{
			Instructions:      "be terse",
			Modalities:        []Modality{ModalityText, ModalityAudio},
			InputAudioFormat:  &format,
			OutputAudioFormat: &format,
			Voice:             &voice,
			Tools:             tools,
			Extensions: Extensions{testExtension{
				provider: "fake",
				id:       "realtime_options",
				fields:   []ExtensionField{"turn_detection"},
			}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, ok := tc.request.(interface{ ActiveFields() []FieldID })
			if !ok {
				t.Fatalf("%T does not implement ActiveFields", tc.request)
			}
			got := provider.ActiveFields()
			want := reflectActiveFields(tc.request)
			// The reflective scan only derives top-level ledger fields; the
			// hand-written implementations then append derived fields (part
			// kinds, extensions). The scan result must therefore be the
			// prefix of the hand-written result, in the same order.
			if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
				t.Errorf("ActiveFields() = %v, want reflective scan %v as prefix", got, want)
			}
		})
	}

	// Every ledger-tagged field on the four request types must be reachable
	// from the hand-written ActiveFields implementations: a field added to a
	// struct without an ActiveFields case (or without a hand-written append)
	// would silently never be active.
	expected := map[FieldID]bool{
		FieldEmbedItems:                false,
		FieldEmbedDimensions:           false,
		FieldTranscriptionAudio:        false,
		FieldTranscriptionLanguage:     false,
		FieldTranscriptionPrompt:       false,
		FieldTranscriptionTimestamps:   false,
		FieldTranscriptionInputFormat:  false,
		FieldRealtimeInstructions:      false,
		FieldRealtimeModalities:        false,
		FieldRealtimeInputAudioFormat:  false,
		FieldRealtimeOutputAudioFormat: false,
		FieldRealtimeVoice:             false,
		FieldRealtimeTools:             false,
	}
	for _, tc := range cases {
		provider := tc.request.(interface{ ActiveFields() []FieldID })
		for _, field := range provider.ActiveFields() {
			if _, ok := expected[field]; ok {
				expected[field] = true
			}
		}
	}
	for field, seen := range expected {
		if !seen {
			t.Errorf("ledger field %s is never produced by a hand-written ActiveFields case", field)
		}
	}
}
