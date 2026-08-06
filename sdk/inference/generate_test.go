package inference

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func TestGenerateRequestSeparatesContextAndUniqueInput(t *testing.T) {
	request := GenerateRequest{
		Context: []message.Message{{
			Role:    message.RoleUser,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "earlier"}}},
		}},
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "now"}}},
				Intent:  Intent{Text: &TextIntent{}},
			},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	missingInput := request
	missingInput.Input = GenerateInput{}
	if err := missingInput.Validate(); err == nil {
		t.Fatal("Validate accepted a request without its one current input")
	}
	missingIntent := request
	missingIntent.Input.Content.Intent = Intent{}
	if err := missingIntent.Validate(); err == nil {
		t.Fatal("Validate accepted current input without intent")
	}

	badContext := request
	badContext.Context = []message.Message{{Role: message.RoleUser}}
	if err := badContext.Validate(); err == nil {
		t.Fatal("Validate accepted invalid prior context")
	}
}

func TestGenerateInputMessageDiscardsIntent(t *testing.T) {
	input := GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hello"}}},
			Intent: Intent{
				Text: &TextIntent{ReasoningEffort: ReasoningHigh},
			},
		},
	}
	msg := input.Message()
	if msg.Role != message.RoleUser || len(msg.Content.Parts) != 1 {
		t.Fatalf("message.Message() = %#v", msg)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal message: %v", err)
	}
	if strings.Contains(string(data), "intent") {
		t.Fatalf("message.Message leaked intent: %s", data)
	}
}

func TestIntentCombinesTypedControls(t *testing.T) {
	count := 2
	maxTokens := 512
	temperature := 0.4
	intent := Intent{
		Text: &TextIntent{
			Response:        &ResponseFormat{Kind: ResponseText},
			MaxOutputTokens: &maxTokens,
			Tools: []message.Definition{{
				Name:        "search",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			ToolChoice:      &ToolChoice{Kind: ToolChoiceAuto},
			Temperature:     &temperature,
			ReasoningEffort: ReasoningMedium,
		},
		Image: &ImageIntent{
			AspectRatio:  media.AspectRatio("1:1"),
			Count:        &count,
			OutputFormat: media.ImageFormatPNG,
			Delivery:     media.SourceURL,
		},
		Audio: &AudioIntent{
			Voice:  media.VoiceSpec{ID: "alloy"},
			Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
			Count:  &count,
		},
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("combined Intent.Validate: %v", err)
	}
	if err := (Intent{}).Validate(); err == nil {
		t.Fatal("Intent accepted no output-producing intent")
	}
}

func TestGenerateRequestJSONRoundTripAndClone(t *testing.T) {
	audio, err := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	duration := int64(750)
	format := media.AudioFormat{Encoding: media.AudioEncodingMP3}
	maxTokens := 64
	request := GenerateRequest{
		Context: []message.Message{{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{message.AudioPart{
				Source:         audio,
				Format:         &format,
				DurationMillis: &duration,
			}}},
		}},
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "continue"},
					message.DataPart{Value: json.RawMessage(`{"mutable":true}`)},
				}},
				Intent: Intent{
					Text: &TextIntent{
						MaxOutputTokens: &maxTokens,
						Tools: []message.Definition{{
							Name:        "search",
							InputSchema: json.RawMessage(`{"type":"object"}`),
						}},
					},
				},
			},
		},
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded GenerateRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("round-tripped Validate: %v", err)
	}
	gotAudio, ok := decoded.Context[0].Content.Parts[0].(message.AudioPart)
	if !ok || gotAudio.Format == nil || gotAudio.DurationMillis == nil ||
		*gotAudio.DurationMillis != duration {
		t.Fatalf("round-tripped audio = %#v", decoded.Context[0].Content.Parts[0])
	}

	clone := decoded.Clone()
	clone.Input.Content.Parts[1].(message.DataPart).Value[0] = '['
	*clone.Input.Content.Intent.Text.MaxOutputTokens = 1
	clone.Input.Content.Intent.Text.Tools[0].InputSchema[0] = '['
	if string(decoded.Input.Content.Parts[1].(message.DataPart).Value) != `{"mutable":true}` {
		t.Fatal("GenerateRequest.Clone shared part payload")
	}
	if *decoded.Input.Content.Intent.Text.MaxOutputTokens != 64 {
		t.Fatal("GenerateRequest.Clone shared intent pointer")
	}
	if string(decoded.Input.Content.Intent.Text.Tools[0].InputSchema) !=
		`{"type":"object"}` {
		t.Fatal("GenerateRequest.Clone shared tool schema")
	}
}

func TestGenerateToolInput(t *testing.T) {
	request := GenerateRequest{
		Input: GenerateInput{
			Role: InputRoleTool,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{message.ToolResultPart{Result: message.Result{
					CallID:  "call-1",
					Content: "found",
				}}}},
				Intent: Intent{
					Text: &TextIntent{ToolChoice: &ToolChoice{Kind: ToolChoiceNone}},
				},
			},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("tool input Validate: %v", err)
	}
	request.Input.Role = InputRoleUser
	if err := request.Validate(); err == nil {
		t.Fatal("user input accepted a tool result")
	}
}

func TestGenerateTextIntentToolRules(t *testing.T) {
	definitions := []message.Definition{{
		Name:        "search",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	choice := func(kind ToolChoiceKind) *ToolChoice {
		if kind == "" {
			return nil
		}
		return &ToolChoice{Kind: kind}
	}
	tests := []struct {
		name    string
		tools   []message.Definition
		choice  *ToolChoice
		wantErr bool
	}{
		{name: "definitions_only"},
		{name: "definitions_auto", choice: choice(ToolChoiceAuto)},
		{name: "definitions_none", choice: choice(ToolChoiceNone)},
		{name: "definitions_required", choice: choice(ToolChoiceRequired)},
		{name: "definitions_named", choice: &ToolChoice{Kind: ToolChoiceNamed, Name: "search"}},
		{name: "named_undefined", choice: &ToolChoice{Kind: ToolChoiceNamed, Name: "missing"}, wantErr: true},
		{name: "choice_without_definitions", choice: choice(ToolChoiceAuto), wantErr: true},
		{name: "none_without_definitions", choice: choice(ToolChoiceNone)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := tt.tools
			if tools == nil && tt.name != "choice_without_definitions" && tt.name != "none_without_definitions" {
				tools = definitions
			}
			intent := Intent{Text: &TextIntent{
				Tools:      tools,
				ToolChoice: tt.choice,
			}}
			err := intent.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Intent.Validate error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGenerateActiveFieldsCoverNestedLedger(t *testing.T) {
	image, _ := media.NewImageURL("https://example.com/cat.png", "image/png")
	audio, _ := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	video, _ := media.NewVideoURL("https://example.com/video.mp4", "video/mp4")
	call, _ := message.NewCall("call-1", "search", map[string]any{"query": "cat"})
	size := media.ImageSize{Width: 512, Height: 768}
	count := 2
	seed := int64(7)
	speed := 1.25
	videoDuration := int64(5000)
	watermark := true
	maxTokens := 128
	temperature := 0.4
	topP := 0.9
	request := GenerateRequest{
		Context: []message.Message{
			{Role: message.RoleUser, Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "hello"},
				message.ImagePart{Source: image},
				message.AudioPart{Source: audio},
				message.VideoPart{Source: video},
				message.FilePart{URI: "s3://bucket/file"},
				message.DataPart{Value: json.RawMessage(`{"x":1}`)},
			}}},
			{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}}},
			{Role: message.RoleTool, Content: message.Content{Parts: []message.Part{message.ToolResultPart{Result: message.Result{CallID: "call-1"}}}}},
		},
		Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "now"},
					message.ImagePart{Source: image},
					message.AudioPart{Source: audio},
					message.VideoPart{Source: video},
					message.FilePart{URI: "s3://bucket/input"},
					message.DataPart{Value: json.RawMessage(`{"y":2}`)},
					message.ToolCallPart{Call: call},
					message.ToolResultPart{Result: message.Result{CallID: "call-1"}},
				}},
				Intent: Intent{
					Text: &TextIntent{
						Response: &ResponseFormat{
							Kind: ResponseJSONSchema, Name: "answer",
							Schema: json.RawMessage(`{"type":"object"}`),
						},
						MaxOutputTokens: &maxTokens,
						Tools: []message.Definition{{
							Name: "search", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`),
						}},
						ToolChoice:      &ToolChoice{Kind: ToolChoiceNamed, Name: "search"},
						Temperature:     &temperature,
						TopP:            &topP,
						ReasoningEffort: ReasoningHigh,
					},
					Image: &ImageIntent{
						Size: &size, AspectRatio: media.AspectRatio("2:3"),
						Count: &count, Seed: &seed,
						OutputFormat: media.ImageFormatPNG, Delivery: media.SourceURL,
					},
					Audio: &AudioIntent{
						Voice: media.VoiceSpec{ID: "alloy", Language: "en"},
						Format: media.AudioFormat{
							Encoding: media.AudioEncodingPCM16, SampleRateHz: 24_000, Channels: 1,
						},
						Speed: &speed, Count: &count,
					},
					Video: &VideoIntent{
						DurationMillis: &videoDuration, Resolution: "720p",
						AspectRatio: media.AspectRatio("16:9"),
						Seed:        &seed, Watermark: &watermark,
					},
				},
			},
		},
		Extensions: Extensions{testExtension{
			provider: "openai", id: "generate_options",
			fields: []ExtensionField{"store"},
		}},
	}
	got := request.ActiveFields()
	want := []FieldID{
		FieldGenerateContextRole,
		FieldGenerateContextText, FieldGenerateContextImage, FieldGenerateContextAudio,
		FieldGenerateContextVideo, FieldGenerateContextFile, FieldGenerateContextData,
		FieldGenerateContextToolCall, FieldGenerateContextToolResult,
		FieldGenerateInputRole,
		FieldGenerateInputText, FieldGenerateInputImage, FieldGenerateInputAudio,
		FieldGenerateInputVideo, FieldGenerateInputFile, FieldGenerateInputData,
		FieldGenerateInputToolCall, FieldGenerateInputToolResult,
		FieldGenerateIntentText, FieldGenerateIntentTextResponse,
		FieldGenerateIntentTextResponseKind, FieldGenerateIntentTextResponseName,
		FieldGenerateIntentTextResponseSchema, FieldGenerateIntentTextMaxOutputTokens,
		FieldGenerateIntentTools,
		FieldGenerateIntentToolChoice, FieldGenerateIntentToolChoiceKind,
		FieldGenerateIntentToolChoiceName,
		FieldGenerateIntentTemperature, FieldGenerateIntentTopP,
		FieldGenerateIntentReasoning, FieldGenerateIntentReasoningEffort,
		FieldGenerateIntentImage, FieldGenerateIntentImageSize,
		FieldGenerateIntentImageSizeWidth, FieldGenerateIntentImageSizeHeight,
		FieldGenerateIntentImageAspectRatio,
		FieldGenerateIntentImageCount, FieldGenerateIntentImageSeed,
		FieldGenerateIntentImageOutputFormat, FieldGenerateIntentImageDelivery,
		FieldGenerateIntentAudio, FieldGenerateIntentAudioVoice,
		FieldGenerateIntentAudioVoiceID, FieldGenerateIntentAudioVoiceLanguage,
		FieldGenerateIntentAudioFormat, FieldGenerateIntentAudioFormatEncoding,
		FieldGenerateIntentAudioFormatSampleRate, FieldGenerateIntentAudioFormatChannels,
		FieldGenerateIntentAudioSpeed, FieldGenerateIntentAudioCount,
		FieldGenerateIntentVideo, FieldGenerateIntentVideoDuration,
		FieldGenerateIntentVideoResolution, FieldGenerateIntentVideoAspectRatio,
		FieldGenerateIntentVideoSeed, FieldGenerateIntentVideoWatermark,
		"extension.openai.generate_options.store",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ActiveFields() =\n%q\nwant\n%q", got, want)
	}
}

func TestGenerateActiveFieldsIncludeExecutionShape(t *testing.T) {
	request := validGenerateTextRequest()
	for _, test := range []struct {
		shape GenerateExecutionShape
		field FieldID
	}{
		{GenerateExecutionUnary, FieldGenerateExecutionUnary},
		{GenerateExecutionStream, FieldGenerateExecutionStream},
	} {
		active := request.ActiveFieldsFor(test.shape)
		if got := active[len(active)-1]; got != test.field {
			t.Fatalf("ActiveFieldsFor(%q) shape field = %q, want %q", test.shape, got, test.field)
		}
	}
}

func TestGenerateResponseCloneOwnsMutableState(t *testing.T) {
	image, _ := media.NewImageBytes([]byte("image"), "image/png")
	generated := int64(1)
	response := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.ImagePart{Source: image},
			message.DataPart{Value: json.RawMessage(`{"answer":42}`)},
		}}},
		FinishReason: FinishCompleted,
		Usage: Usage{
			InputTokens: 1, OutputTokens: 2, TotalTokens: 3,
			GeneratedImages: &generated,
			Input:           InputTokenUsage{CacheReadTokens: count(0)},
		},
		Metadata: Metadata{Decisions: []Decision{{Field: FieldGenerateInputRole, Disposition: Native}}},
	}
	clone := response.Clone()
	clone.Message.Content.Parts[1].(message.DataPart).Value[0] = '['
	*clone.Usage.GeneratedImages = 2
	*clone.Usage.Input.CacheReadTokens = 3
	clone.Metadata.Decisions[0].Disposition = Rejected
	if string(response.Message.Content.Parts[1].(message.DataPart).Value) != `{"answer":42}` ||
		*response.Usage.GeneratedImages != 1 || *response.Usage.Input.CacheReadTokens != 0 ||
		response.Metadata.Decisions[0].Disposition != Native {
		t.Fatal("GenerateResponse.Clone shared mutable response state")
	}
}

func TestGenerateResponseValidateForCompletedCombinedIntent(t *testing.T) {
	image, _ := media.NewImageURL("https://example.com/cat.png", "image/png")
	audio, _ := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	count := 1
	request := GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "make it"}}},
			Intent: Intent{
				Text: &TextIntent{Response: &ResponseFormat{
					Kind: ResponseJSONSchema, Name: "answer",
					Schema: json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}`),
				}},
				Image: &ImageIntent{Count: &count, OutputFormat: media.ImageFormatPNG, Delivery: media.SourceURL},
				Audio: &AudioIntent{
					Voice:  media.VoiceSpec{ID: "alloy"},
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
					Count:  &count,
				},
			},
		},
	}}
	response := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: `{"answer":42}`},
			message.ImagePart{Source: image},
			message.AudioPart{Source: audio, Format: &media.AudioFormat{Encoding: media.AudioEncodingMP3}},
		}}},
		FinishReason: FinishCompleted,
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("valid combined response rejected: %v", err)
	}

	badSchema := response
	badSchema.Message = response.Message.Clone()
	badSchema.Message.Content.Parts[0] = message.TextPart{Text: `{"answer":"wrong"}`}
	if err := badSchema.ValidateFor(request); err == nil {
		t.Fatal("completed schema-invalid text accepted")
	}
	badDelivery := response
	badDelivery.Message = response.Message.Clone()
	inline, _ := media.NewImageBytes([]byte("image"), "image/png")
	badDelivery.Message.Content.Parts[1] = message.ImagePart{Source: inline}
	if err := badDelivery.ValidateFor(request); err == nil {
		t.Fatal("completed image with wrong delivery accepted")
	}
	badAudio := response
	badAudio.Message = response.Message.Clone()
	badAudio.Message.Content.Parts[2] = message.AudioPart{
		Source: audio, Format: &media.AudioFormat{Encoding: media.AudioEncodingAAC},
	}
	if err := badAudio.ValidateFor(request); err == nil {
		t.Fatal("completed audio with wrong format accepted")
	}
	tooManyImages := response
	tooManyImages.Message = response.Message.Clone()
	tooManyImages.Message.Content.Parts = append(
		tooManyImages.Message.Content.Parts, message.ImagePart{Source: image},
	)
	if err := tooManyImages.ValidateFor(request); err == nil {
		t.Fatal("completed image cardinality mismatch accepted")
	}
}

func TestGenerateResponsePartialSkipsCompletenessButRejectsWrongModality(t *testing.T) {
	count := 2
	request := GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "make it"}}},
			Intent: Intent{
				Text:  &TextIntent{Response: &ResponseFormat{Kind: ResponseJSONObject}},
				Image: &ImageIntent{Count: &count},
			},
		},
	}}
	partial := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: `{"answer":`},
		}}},
		FinishReason: FinishMaxOutput,
	}
	if err := partial.ValidateFor(request); err != nil {
		t.Fatalf("partial response was subjected to completeness checks: %v", err)
	}
	negative := int64(-1)
	invalidUsage := partial
	invalidUsage.Usage.InputCharacters = &negative
	if err := invalidUsage.ValidateFor(request); err == nil {
		t.Fatal("partial response with invalid usage accepted")
	}
	audio, _ := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	partial.Message.Content.Parts = append(partial.Message.Content.Parts, message.AudioPart{Source: audio})
	if err := partial.ValidateFor(request); err == nil {
		t.Fatal("partial response with unrequested modality accepted")
	}
}

func TestGenerateResponseToolFinishParity(t *testing.T) {
	call, _ := message.NewCall("call-1", "search", map[string]any{"query": "cat"})
	response := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.ToolCallPart{Call: call},
		}}},
		FinishReason: FinishCompleted,
	}
	if err := response.Validate(); err == nil {
		t.Fatal("completed response with tool calls accepted")
	}
	response.FinishReason = FinishToolCalls
	if err := response.Validate(); err != nil {
		t.Fatalf("tool-call finish rejected: %v", err)
	}
	response.Message.Content.Parts = []message.Part{message.TextPart{Text: "no call"}}
	if err := response.Validate(); err == nil {
		t.Fatal("tool-call finish without tool calls accepted")
	}
}

func TestUnifiedFinishReasonValues(t *testing.T) {
	reasons := map[FinishReason]string{
		FinishCompleted:       "completed",
		FinishMaxOutput:       "max_output",
		FinishToolCalls:       "tool_calls",
		FinishContentFilter:   "content_filter",
		FinishRefusal:         "refusal",
		FinishPause:           "pause",
		FinishInvalidToolCall: "invalid_tool_call",
		FinishContextLimit:    "context_limit",
		FinishOther:           "other",
	}
	for reason, want := range reasons {
		if string(reason) != want {
			t.Errorf("finish reason %q = %q, want %q", want, reason, want)
		}
	}
}

func TestGenerateResponseValidateForEnforcesToolChoiceAndDefinitions(t *testing.T) {
	definitions := []message.Definition{
		{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	call := func(name string) message.Part {
		value, err := message.NewCall("call-1", name, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		return message.ToolCallPart{Call: value}
	}
	response := func(parts ...message.Part) GenerateResponse {
		return GenerateResponse{
			Message:      message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: parts}},
			FinishReason: FinishToolCalls,
		}
	}
	request := func(choice ToolChoice) GenerateRequest {
		return GenerateRequest{Input: GenerateInput{
			Role: InputRoleUser,
			Content: InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: "use a tool"}}},
				Intent: Intent{
					Text: &TextIntent{
						Tools:      definitions,
						ToolChoice: &choice,
					},
				},
			},
		}}
	}

	tests := []struct {
		name     string
		choice   ToolChoice
		response GenerateResponse
		wantErr  bool
	}{
		{"none_rejects_call", ToolChoice{Kind: ToolChoiceNone}, response(call("search")), true},
		{"required_requires_call", ToolChoice{Kind: ToolChoiceRequired}, GenerateResponse{
			Message: message.Message{Role: message.RoleAssistant, Content: message.Content{
				Parts: []message.Part{message.TextPart{Text: "no call"}},
			}},
			FinishReason: FinishCompleted,
		}, true},
		{"named_accepts_only_name", ToolChoice{Kind: ToolChoiceNamed, Name: "search"}, response(call("fetch")), true},
		{"undefined_call_rejected", ToolChoice{Kind: ToolChoiceAuto}, response(call("missing")), true},
		{"required_accepts_defined_call", ToolChoice{Kind: ToolChoiceRequired}, response(call("search")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.response.ValidateFor(request(tt.choice))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateFor error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGenerateResponseAllowsEmptyMessageOnlyForIncompleteFinish(t *testing.T) {
	request := validGenerateTextRequest()
	incomplete := []FinishReason{
		FinishContentFilter, FinishRefusal, FinishContextLimit, FinishPause,
		FinishMaxOutput, FinishInvalidToolCall, FinishOther,
	}
	for _, reason := range incomplete {
		t.Run(string(reason), func(t *testing.T) {
			response := GenerateResponse{
				Message:      message.Message{Role: message.RoleAssistant},
				FinishReason: reason,
			}
			if err := response.ValidateFor(request); err != nil {
				t.Fatalf("empty incomplete response rejected: %v", err)
			}
		})
	}
	required := request.Clone()
	required.Input.Content.Intent.Text.Tools = []message.Definition{{
		Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	required.Input.Content.Intent.Text.ToolChoice = &ToolChoice{Kind: ToolChoiceRequired}
	if err := (GenerateResponse{
		Message:      message.Message{Role: message.RoleAssistant},
		FinishReason: FinishPause,
	}).ValidateFor(required); err != nil {
		t.Fatalf("empty paused required-tool response rejected: %v", err)
	}
	for _, reason := range []FinishReason{FinishCompleted, FinishToolCalls} {
		t.Run(string(reason), func(t *testing.T) {
			response := GenerateResponse{
				Message:      message.Message{Role: message.RoleAssistant},
				FinishReason: reason,
			}
			if err := response.ValidateFor(request); err == nil {
				t.Fatal("empty complete response accepted")
			}
		})
	}
}

func TestResponseFormatValidateCompilesJSONSchema(t *testing.T) {
	format := ResponseFormat{
		Kind:   ResponseJSONSchema,
		Name:   "broken",
		Schema: json.RawMessage(`{"type":"definitely-not-a-json-schema-type"}`),
	}
	if err := format.Validate(); err == nil {
		t.Fatal("syntactically valid but uncompilable JSON schema accepted")
	}
}

func TestVideoIntentValidationAndClone(t *testing.T) {
	duration := int64(5000)
	seed := int64(42)
	watermark := false
	valid := VideoIntent{
		DurationMillis: &duration, Resolution: "1080p",
		AspectRatio: media.AspectRatio("9:16"), Seed: &seed, Watermark: &watermark,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid VideoIntent rejected: %v", err)
	}
	if err := (Intent{Video: &valid}).Validate(); err != nil {
		t.Fatalf("video-only Intent rejected: %v", err)
	}

	zero := int64(0)
	for name, intent := range map[string]VideoIntent{
		"non-positive duration": {DurationMillis: &zero},
		"bad resolution token":  {Resolution: "hd"},
		"bad aspect ratio":      {AspectRatio: media.AspectRatio("widescreen")},
	} {
		if err := intent.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	clone := valid.Clone()
	*clone.DurationMillis = 9000
	*clone.Watermark = true
	if *valid.DurationMillis != 5000 || *valid.Watermark {
		t.Fatal("VideoIntent.Clone shared a pointer")
	}
}

func TestGenerateResponseValidateForVideo(t *testing.T) {
	video, err := media.NewVideoURL("https://example.com/out.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	videoRequest := GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "a cat walking"}}},
			Intent:  Intent{Video: &VideoIntent{}},
		},
	}}
	response := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.VideoPart{Source: video},
		}}},
		FinishReason: FinishCompleted,
	}
	if err := response.ValidateFor(videoRequest); err != nil {
		t.Fatalf("valid video response rejected: %v", err)
	}
	derived := response
	deriveGenerateUsage(videoRequest, &derived)
	if derived.Usage.GeneratedVideos == nil || *derived.Usage.GeneratedVideos != 1 {
		t.Fatalf("GeneratedVideos = %v, want 1", derived.Usage.GeneratedVideos)
	}

	textRequest := GenerateRequest{Input: GenerateInput{
		Role: InputRoleUser,
		Content: InputContent{
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
			Intent:  Intent{Text: &TextIntent{}},
		},
	}}
	if err := response.ValidateFor(textRequest); err == nil {
		t.Fatal("unrequested video part accepted")
	}
	textResponse := GenerateResponse{
		Message: message.Message{Role: message.RoleAssistant, Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "hi"},
		}}},
		FinishReason: FinishCompleted,
	}
	deriveGenerateUsage(textRequest, &textResponse)
	if textResponse.Usage.GeneratedVideos != nil {
		t.Fatalf("text-only response derived GeneratedVideos = %v", textResponse.Usage.GeneratedVideos)
	}

	missing := response
	missing.Message = message.Message{Role: message.RoleAssistant}
	if err := missing.ValidateFor(videoRequest); err == nil {
		t.Fatal("completed response without the requested video accepted")
	}
	unfinished := response
	unfinished.FinishReason = FinishMaxOutput
	unfinished.Message = message.Message{Role: message.RoleAssistant}
	if err := unfinished.ValidateFor(videoRequest); err != nil {
		t.Fatalf("unfinished response without video rejected: %v", err)
	}
}
