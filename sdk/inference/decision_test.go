package inference

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestCompileReportRequiresCompleteNativeDisposition(t *testing.T) {
	active := []FieldID{
		FieldGenerateExecutionUnary,
		FieldGenerateInputRole,
		FieldGenerateInputText,
	}
	report := CompileReport{
		Operation: OperationGenerate,
		Decisions: []Decision{
			{Field: FieldGenerateExecutionUnary, Disposition: Native},
			{Field: FieldGenerateInputRole, Disposition: Native},
		},
	}
	if err := report.ValidateSuccess(OperationGenerate, active); err == nil {
		t.Fatal("expected missing-field contract violation")
	} else if !IsKind(err, CompilerContractViolation) || !errdefs.IsInternal(err) {
		t.Fatalf("error = %v, want internal CompilerContractViolation", err)
	}

	report.Decisions = append(report.Decisions, Decision{
		Field:       FieldGenerateInputText,
		Disposition: Disposition("emulated"),
		Reason:      "temperature cannot be encoded",
	})
	if err := report.ValidateSuccess(OperationGenerate, active); err == nil {
		t.Fatal("expected non-native disposition contract violation")
	}
}

func TestCompileReportDroppedDisposition(t *testing.T) {
	active := []FieldID{
		FieldGenerateExecutionUnary,
		FieldGenerateInputRole,
		FieldGenerateContextReasoning,
	}

	dropped := CompileReport{
		Operation: OperationGenerate,
		Decisions: []Decision{
			{Field: FieldGenerateExecutionUnary, Disposition: Native},
			{Field: FieldGenerateInputRole, Disposition: Native},
			{
				Field:       FieldGenerateContextReasoning,
				Disposition: Dropped,
				Reason:      "provider does not consume reasoning input",
			},
		},
	}
	if err := dropped.ValidateSuccess(OperationGenerate, active); err != nil {
		t.Fatalf("dropped field with reason must validate: %v", err)
	}

	reasonless := dropped.Clone()
	reasonless.Decisions[2].Reason = ""
	if err := reasonless.ValidateSuccess(OperationGenerate, active); err == nil {
		t.Fatal("dropped field without reason must be a contract violation")
	} else if !IsKind(err, CompilerContractViolation) {
		t.Fatalf("error = %v, want CompilerContractViolation", err)
	}
}

func TestRequestFieldsDeclareLedgerMetadata(t *testing.T) {
	known := map[FieldID]struct{}{
		FieldGenerateExecutionUnary: {}, FieldGenerateExecutionStream: {},
		FieldGenerateContextRole: {}, FieldGenerateContextText: {},
		FieldGenerateContextImage: {}, FieldGenerateContextAudio: {},
		FieldGenerateContextVideo: {}, FieldGenerateContextFile: {},
		FieldGenerateContextData: {}, FieldGenerateContextToolCall: {},
		FieldGenerateContextToolResult: {}, FieldGenerateInputRole: {},
		FieldGenerateInputText: {}, FieldGenerateInputImage: {},
		FieldGenerateInputAudio: {}, FieldGenerateInputVideo: {},
		FieldGenerateInputFile: {}, FieldGenerateInputData: {},
		FieldGenerateInputToolCall: {}, FieldGenerateInputToolResult: {},
		FieldGenerateContextReasoning: {}, FieldGenerateInputReasoning: {},
		FieldGenerateIntentText: {}, FieldGenerateIntentTextResponse: {},
		FieldGenerateIntentTextResponseKind:    {},
		FieldGenerateIntentTextResponseName:    {},
		FieldGenerateIntentTextResponseSchema:  {},
		FieldGenerateIntentTextMaxOutputTokens: {},
		FieldGenerateIntentImage:               {}, FieldGenerateIntentImageSize: {},
		FieldGenerateIntentImageSizeWidth:   {},
		FieldGenerateIntentImageSizeHeight:  {},
		FieldGenerateIntentImageAspectRatio: {},
		FieldGenerateIntentImageCount:       {}, FieldGenerateIntentImageSeed: {},
		FieldGenerateIntentImageOutputFormat: {},
		FieldGenerateIntentImageDelivery:     {},
		FieldGenerateIntentAudio:             {}, FieldGenerateIntentAudioVoice: {},
		FieldGenerateIntentAudioVoiceID:          {},
		FieldGenerateIntentAudioVoiceLanguage:    {},
		FieldGenerateIntentAudioFormat:           {},
		FieldGenerateIntentAudioFormatEncoding:   {},
		FieldGenerateIntentAudioFormatSampleRate: {},
		FieldGenerateIntentAudioFormatChannels:   {},
		FieldGenerateIntentAudioSpeed:            {}, FieldGenerateIntentAudioCount: {},
		FieldGenerateIntentVideo: {}, FieldGenerateIntentVideoDuration: {},
		FieldGenerateIntentVideoResolution:  {},
		FieldGenerateIntentVideoAspectRatio: {},
		FieldGenerateIntentVideoSeed:        {}, FieldGenerateIntentVideoWatermark: {},
		FieldGenerateIntentTools:            {},
		FieldGenerateIntentToolChoice:       {},
		FieldGenerateIntentToolChoiceKind:   {},
		FieldGenerateIntentToolChoiceName:   {},
		FieldGenerateIntentTemperature:      {},
		FieldGenerateIntentTopP:             {},
		FieldGenerateIntentReasoning:        {},
		FieldGenerateIntentReasoningEnabled: {},
		FieldGenerateIntentReasoningEffort:  {},
		FieldEmbedItems:                     {}, FieldEmbedDimensions: {},
		FieldTranscriptionAudio:    {},
		FieldTranscriptionLanguage: {}, FieldTranscriptionPrompt: {},
		FieldTranscriptionTimestamps:  {},
		FieldTranscriptionInputFormat: {}, FieldRealtimeInstructions: {},
		FieldRealtimeModalities: {}, FieldRealtimeInputAudioFormat: {},
		FieldRealtimeOutputAudioFormat: {}, FieldRealtimeVoice: {},
		FieldRealtimeTools: {}, FieldRealtimeInputText: {},
		FieldRealtimeInputAudio: {}, FieldRealtimeInputVideo: {},
		FieldRealtimeInputToolResult: {},
	}
	for _, requestType := range []reflect.Type{
		reflect.TypeFor[GenerateRequest](),
		reflect.TypeFor[EmbedRequest](),
		reflect.TypeFor[TranscriptionRequest](),
		reflect.TypeFor[TranscriptionSessionConfig](),
		reflect.TypeFor[RealtimeConfig](),
		reflect.TypeFor[RealtimeTextInput](),
		reflect.TypeFor[RealtimeAudioInput](),
		reflect.TypeFor[RealtimeVideoInput](),
		reflect.TypeFor[RealtimeToolResultInput](),
	} {
		t.Run(requestType.Name(), func(t *testing.T) {
			for i := 0; i < requestType.NumField(); i++ {
				field := requestType.Field(i)
				if field.IsExported() && field.Tag.Get("ledger") == "" {
					t.Errorf("exported field %s has no ledger tag", field.Name)
				}
				tag := FieldID(field.Tag.Get("ledger"))
				if tag != "extension" {
					if _, ok := known[tag]; !ok {
						t.Errorf("exported field %s has unknown ledger tag %q", field.Name, tag)
					}
				}
			}
		})
	}
}

func TestCompileReportRequiresEveryActiveExtensionField(t *testing.T) {
	request := validGenerateTextRequest()
	request.Extensions = Extensions{testExtension{
		provider: "openai",
		id:       "generate_options",
		fields:   []ExtensionField{"service_tier", "store"},
	}}
	active := request.ActiveFieldsFor(GenerateExecutionUnary)
	decisions := make([]Decision, 0, len(active)-1)
	for _, field := range active[:len(active)-1] {
		decisions = append(decisions, Decision{Field: field, Disposition: Native})
	}
	report := CompileReport{
		Operation: OperationGenerate,
		Decisions: decisions,
	}
	if err := report.ValidateSuccess(OperationGenerate, active); err == nil {
		t.Fatal("expected missing extension field disposition")
	}
}
