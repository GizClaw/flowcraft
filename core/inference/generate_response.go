package inference

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/message"
)

type GenerateResponse struct {
	Message      message.Message `json:"message"`
	FinishReason FinishReason    `json:"finish_reason"`
	// FinishSynthesized marks a finish reason the adapter fabricated
	// because the provider ended the stream without emitting a terminal
	// finish event (false for provider-emitted reasons). A synthesized
	// finish may mean the response was truncated mid-flight; callers that
	// need to trust the response as complete can use the flag to decide
	// whether to treat the attempt as void and re-initiate at their own
	// granularity.
	FinishSynthesized bool     `json:"finish_synthesized,omitempty"`
	Usage             Usage    `json:"usage"`
	Metadata          Metadata `json:"metadata"`
	// ProviderOutputs carries provider-owned structured output that is not
	// part of Message. It is never fed back into a model request implicitly;
	// only Message becomes conversation context.
	ProviderOutputs ProviderOutputs `json:"provider_outputs,omitempty"`
}

func (r GenerateResponse) Clone() GenerateResponse {
	r.Message = r.Message.Clone()
	r.Usage = r.Usage.Clone()
	r.Metadata.Decisions = cloneDecisions(r.Metadata.Decisions)
	r.ProviderOutputs = r.ProviderOutputs.Clone()
	return r
}

func (r GenerateResponse) Validate() error {
	check, _, err := r.validate()
	return responseCheckError(check, err)
}

func (r GenerateResponse) validate() (generateResponseCheck, []message.Part, error) {
	if r.Message.Role != message.RoleAssistant {
		return generateResponseCheckMessageRole, nil,
			fmt.Errorf("generate response message must have assistant role")
	}
	if err := r.FinishReason.Validate(); err != nil {
		return generateResponseCheckFinishReason, nil, err
	}
	var parts []message.Part
	if len(r.Message.Content.Parts) == 0 {
		if r.FinishReason == FinishCompleted || r.FinishReason == FinishToolCalls {
			return generateResponseCheckEmptyContent, nil, fmt.Errorf(
				"generate response content is required for finish reason %q",
				r.FinishReason,
			)
		}
	} else {
		// Normalize every part before message validation so structural failures
		// take precedence over invalid media or other message-level checks.
		parts = make([]message.Part, 0, len(r.Message.Content.Parts))
		for i, part := range r.Message.Content.Parts {
			normalized, err := message.NormalizePart(part)
			if err != nil {
				return generateResponseCheckPart, nil, fmt.Errorf("content part %d: %w", i, err)
			}
			parts = append(parts, normalized)
		}
		if err := r.Message.Validate(); err != nil {
			return responseMessageValidationCheck(parts), nil, err
		}
	}
	if err := r.Usage.Validate(); err != nil {
		return generateResponseCheckUsage, nil, err
	}
	if err := r.ProviderOutputs.Validate(); err != nil {
		return generateResponseCheckProviderOutput, nil, err
	}
	hasToolCalls := r.Message.HasToolCalls()
	if (r.FinishReason == FinishToolCalls) != hasToolCalls {
		return generateResponseCheckMismatch, nil,
			fmt.Errorf("tool-call finish reason does not match response tool calls")
	}
	for _, normalized := range parts {
		switch normalized.(type) {
		case message.TextPart, message.ImagePart, message.AudioPart, message.VideoPart, message.ToolCallPart, message.ReasoningPart:
		default:
			return generateResponseCheckUnsupportedPart, nil,
				fmt.Errorf("generate response contains unsupported part %q", normalized.Kind())
		}
	}
	return "", parts, nil
}

// undefinedToolError reports a tool call whose name is absent from the
// request's tool definitions. ValidateFor returns it instead of a plain
// error so callers can distinguish "the model called a tool it was never
// shown" (deterministic, potentially recoverable via tool_search) from
// other response corruption.
type undefinedToolError struct {
	Index int
	Call  message.ToolCall
}

func (e *undefinedToolError) Error() string {
	return fmt.Sprintf(
		"generate tool call %d names undefined tool %s", e.Index, quoteToken(e.Call.Name))
}

// redactedCause: the message carries the model's tool name, which reaches
// the log only through quoteToken's bounded, quoted rendering.
func (*undefinedToolError) redactedCause() {}

// generateResponseCheckError carries the static validation stage alongside
// the original cause. The stage is safe to expose through Error.Detail; the
// cause remains available to callers that need errors.Is/errors.As.
type generateResponseCheckError struct {
	check generateResponseCheck
	err   error
}

func (e *generateResponseCheckError) Error() string { return e.err.Error() }
func (e *generateResponseCheckError) Unwrap() error { return e.err }

func (*generateResponseCheckError) redactedCause() {}

type generateResponseCheck string

const (
	generateResponseCheckMessageRole      generateResponseCheck = "message_role"
	generateResponseCheckFinishReason     generateResponseCheck = "finish_reason"
	generateResponseCheckEmptyContent     generateResponseCheck = "empty_content"
	generateResponseCheckMessage          generateResponseCheck = "message"
	generateResponseCheckUsage            generateResponseCheck = "usage"
	generateResponseCheckProviderOutput   generateResponseCheck = "provider_output"
	generateResponseCheckMismatch         generateResponseCheck = "mismatch"
	generateResponseCheckPart             generateResponseCheck = "part"
	generateResponseCheckUnsupportedPart  generateResponseCheck = "unsupported_part"
	generateResponseCheckToolCall         generateResponseCheck = "tool_call"
	generateResponseCheckUnrequestedText  generateResponseCheck = "unrequested_part.text"
	generateResponseCheckUnrequestedImage generateResponseCheck = "unrequested_part.image"
	generateResponseCheckUnrequestedAudio generateResponseCheck = "unrequested_part.audio"
	generateResponseCheckUnrequestedVideo generateResponseCheck = "unrequested_part.video"
	generateResponseCheckImageMedia       generateResponseCheck = "media.image"
	generateResponseCheckAudioMedia       generateResponseCheck = "media.audio"
	generateResponseCheckVideoMedia       generateResponseCheck = "media.video"
	generateResponseCheckUnrequestedTool  generateResponseCheck = "unrequested_tool"
	generateResponseCheckUndefinedTool    generateResponseCheck = "undefined_tool"
	generateResponseCheckToolChoice       generateResponseCheck = "tool_choice"
	generateResponseCheckNoText           generateResponseCheck = "no_text"
	generateResponseCheckText             generateResponseCheck = "text"
	generateResponseCheckImageCount       generateResponseCheck = "count.image"
	generateResponseCheckAudioCount       generateResponseCheck = "count.audio"
	generateResponseCheckVideoCount       generateResponseCheck = "count.video"
)

func responseCheckError(check generateResponseCheck, err error) error {
	if err == nil {
		return nil
	}
	return &generateResponseCheckError{check: check, err: err}
}

func (r GenerateResponse) ValidateFor(request GenerateRequest) error {
	check, err := r.validateFor(request)
	return responseCheckError(check, err)
}

func (r GenerateResponse) validateFor(request GenerateRequest) (generateResponseCheck, error) {
	deriveGenerateUsage(request, &r)
	check, parts, err := r.validate()
	if err != nil {
		return check, err
	}
	intent := request.Input.Content.Intent
	// toolsRequested decides whether a tool-call part is a requested
	// artifact at all, and whether the definition and choice checks run.
	toolsRequested := intent.Text != nil && intent.Text.toolsRequested()
	collected, check, err := collectGenerateParts(parts, intent, toolsRequested)
	if err != nil {
		return check, err
	}
	if check, err := validateGenerateToolCalls(
		intent.Text, toolsRequested, collected.toolCalls, r.FinishReason,
	); err != nil {
		return check, err
	}
	if r.FinishReason != FinishCompleted {
		return "", nil
	}
	return validateCompletedGenerate(intent, collected)
}

// generateParts is one response's parts classified against the request:
// text accumulated for the format check, media kept for the per-part and
// count checks, tool calls kept for the definition and tool-choice checks.
type generateParts struct {
	text      strings.Builder
	textParts int
	images    []message.ImagePart
	audio     []message.AudioPart
	videos    []message.VideoPart
	toolCalls []message.ToolCallPart
}

// collectGenerateParts walks the response parts once and rejects any part
// kind the request did not ask for, first bad part in source order winning.
// Each media part is validated against its modality's intent as it is
// collected. Reasoning parts pass unconditionally: they are a trace of the
// model's own process, not a requested artifact, so reasoning-capable
// models may always emit them.
func collectGenerateParts(
	parts []message.Part,
	intent Intent,
	toolsRequested bool,
) (generateParts, generateResponseCheck, error) {
	var collected generateParts
	requested := make(map[message.PartKind]struct{}, 4)
	for _, kind := range intent.OutputKinds() {
		requested[kind] = struct{}{}
	}
	for _, part := range parts {
		switch value := part.(type) {
		case message.TextPart:
			if _, ok := requested[message.PartText]; !ok {
				return collected, generateResponseCheckUnrequestedText,
					fmt.Errorf("generate response contains unrequested text")
			}
			collected.textParts++
			collected.text.WriteString(value.Text)
		case message.ImagePart:
			if _, ok := requested[message.PartImage]; !ok {
				return collected, generateResponseCheckUnrequestedImage,
					fmt.Errorf("generate response contains unrequested image")
			}
			collected.images = append(collected.images, value)
			if err := validateGenerateImage(value, *intent.Image); err != nil {
				return collected, generateResponseCheckImageMedia, fmt.Errorf(
					"generate image %d: %w", len(collected.images)-1, err,
				)
			}
		case message.AudioPart:
			if _, ok := requested[message.PartAudio]; !ok {
				return collected, generateResponseCheckUnrequestedAudio,
					fmt.Errorf("generate response contains unrequested audio")
			}
			collected.audio = append(collected.audio, value)
			if err := validateGenerateAudio(value, *intent.Audio); err != nil {
				return collected, generateResponseCheckAudioMedia, fmt.Errorf(
					"generate audio %d: %w", len(collected.audio)-1, err,
				)
			}
		case message.VideoPart:
			if _, ok := requested[message.PartVideo]; !ok {
				return collected, generateResponseCheckUnrequestedVideo,
					fmt.Errorf("generate response contains unrequested video")
			}
			collected.videos = append(collected.videos, value)
			if err := validateGenerateVideo(value); err != nil {
				return collected, generateResponseCheckVideoMedia, fmt.Errorf(
					"generate video %d: %w", len(collected.videos)-1, err,
				)
			}
		case message.ToolCallPart:
			if !toolsRequested {
				return collected, generateResponseCheckUnrequestedTool,
					fmt.Errorf("generate response contains an unrequested tool call")
			}
			collected.toolCalls = append(collected.toolCalls, value)
		case message.ReasoningPart:
		}
	}
	return collected, "", nil
}

// validateGenerateToolCalls checks the declared definitions and the
// tool-choice strategy against the calls the model actually produced. It
// is a no-op when the request asked for no tool calling at all.
func validateGenerateToolCalls(
	text *TextIntent,
	toolsRequested bool,
	calls []message.ToolCallPart,
	finishReason FinishReason,
) (generateResponseCheck, error) {
	if !toolsRequested {
		return "", nil
	}
	definitions := make(map[string]struct{}, len(text.Tools))
	for _, definition := range text.Tools {
		definitions[definition.Name] = struct{}{}
	}
	for index, call := range calls {
		if _, ok := definitions[call.Call.Name]; !ok {
			return generateResponseCheckUndefinedTool,
				&undefinedToolError{Index: index, Call: call.Call}
		}
	}
	return validateGenerateToolChoice(text.ToolChoice, calls, finishReason)
}

// validateGenerateToolChoice enforces the request's selection rule on the
// produced calls: none forbids them, required and named demand one unless
// the model stopped for a non-completion reason (in which case the
// finish-reason checks own the failure).
func validateGenerateToolChoice(
	choice *ToolChoice,
	calls []message.ToolCallPart,
	finishReason FinishReason,
) (generateResponseCheck, error) {
	if choice == nil {
		return "", nil
	}
	switch choice.Kind {
	case ToolChoiceNone:
		if len(calls) != 0 {
			return generateResponseCheckToolChoice,
				fmt.Errorf("tool choice none forbids tool calls")
		}
	case ToolChoiceRequired:
		if len(calls) == 0 && finishReason == FinishCompleted {
			return generateResponseCheckToolChoice,
				fmt.Errorf("required tool choice produced no tool call")
		}
	case ToolChoiceNamed:
		for _, call := range calls {
			if call.Call.Name != choice.Name {
				return generateResponseCheckToolChoice, fmt.Errorf(
					"named tool choice %s produced tool %s",
					quoteToken(choice.Name),
					quoteToken(call.Call.Name),
				)
			}
		}
		if len(calls) == 0 && finishReason == FinishCompleted {
			return generateResponseCheckToolChoice,
				fmt.Errorf("named tool choice produced no tool call")
		}
	}
	return "", nil
}

// validateCompletedGenerate checks the artifacts a completed response owes
// the request: text where text was asked for, and the declared counts for
// every requested media modality. A response that stopped for any other
// reason is not held to these promises.
func validateCompletedGenerate(
	intent Intent,
	collected generateParts,
) (generateResponseCheck, error) {
	if intent.Text != nil {
		if collected.textParts == 0 {
			return generateResponseCheckNoText,
				fmt.Errorf("completed generate response contains no requested text")
		}
		if err := validateGenerateText(
			collected.text.String(), intent.Text.Response,
		); err != nil {
			return generateResponseCheckText, err
		}
	}
	if intent.Image != nil {
		if err := validateGenerateCount(
			"image", len(collected.images), intent.Image.Count,
		); err != nil {
			return generateResponseCheckImageCount, err
		}
	}
	if intent.Audio != nil {
		if err := validateGenerateCount(
			"audio", len(collected.audio), intent.Audio.Count,
		); err != nil {
			return generateResponseCheckAudioCount, err
		}
	}
	if intent.Video != nil {
		if err := validateGenerateCount(
			"video", len(collected.videos), nil,
		); err != nil {
			return generateResponseCheckVideoCount, err
		}
	}
	return "", nil
}

func responseMessageValidationCheck(parts []message.Part) generateResponseCheck {
	// Keep malformed tool calls on their stable terminal label regardless of
	// position; otherwise, the first invalid part in source order wins.
	var firstCheck generateResponseCheck
	for _, part := range parts {
		if err := part.Validate(); err != nil {
			check := generateResponseCheckMessage
			switch part.(type) {
			case message.ImagePart:
				check = generateResponseCheckImageMedia
			case message.AudioPart:
				check = generateResponseCheckAudioMedia
			case message.VideoPart:
				check = generateResponseCheckVideoMedia
			case message.ToolCallPart:
				return generateResponseCheckToolCall
			}
			if firstCheck == "" {
				firstCheck = check
			}
		}
	}
	if firstCheck != "" {
		return firstCheck
	}
	return generateResponseCheckMessage
}

// validateGenerateVideo checks the output part is genuinely video. Unlike
// image and audio, no canonical video parameter constrains the returned
// encoding beyond the media family.
func validateGenerateVideo(part message.VideoPart) error {
	if mediaType := part.Source.BaseMediaType(); !strings.HasPrefix(mediaType, "video/") {
		return fmt.Errorf(
			"video part media type %s is not video", quoteToken(part.Source.MediaType()))
	}
	return nil
}

func deriveGenerateUsage(request GenerateRequest, response *GenerateResponse) {
	imageCount := int64(0)
	hasImage := false
	hasAudio := false
	audioDurationKnown := true
	audioDuration := int64(0)
	videoCount := int64(0)
	hasVideo := false
	for _, part := range response.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			continue
		}
		switch value := normalized.(type) {
		case message.ImagePart:
			hasImage = true
			imageCount++
		case message.AudioPart:
			hasAudio = true
			if value.DurationMillis == nil {
				audioDurationKnown = false
			} else {
				audioDuration += *value.DurationMillis
			}
		case message.VideoPart:
			hasVideo = true
			videoCount++
		}
	}
	if hasImage || request.Input.Content.Intent.Image != nil {
		response.Usage.GeneratedImages = &imageCount
	} else {
		response.Usage.GeneratedImages = nil
	}
	if hasAudio && audioDurationKnown {
		response.Usage.AudioDurationMillis = &audioDuration
	} else {
		response.Usage.AudioDurationMillis = nil
	}
	if hasVideo || request.Input.Content.Intent.Video != nil {
		response.Usage.GeneratedVideos = &videoCount
	} else {
		response.Usage.GeneratedVideos = nil
	}
}

func validateGenerateCount(name string, actual int, requested *int) error {
	if requested == nil {
		if actual == 0 {
			return fmt.Errorf("completed generate response contains no requested %s", name)
		}
		return nil
	}
	if actual != *requested {
		return fmt.Errorf(
			"generate %s count %d does not match requested count %d",
			name,
			actual,
			*requested,
		)
	}
	return nil
}

func validateGenerateImage(part message.ImagePart, intent ImageIntent) error {
	if intent.Delivery != "" && part.Source.Kind() != intent.Delivery {
		return fmt.Errorf(
			"delivery %q does not match requested delivery %q",
			part.Source.Kind(),
			intent.Delivery,
		)
	}
	if intent.OutputFormat != "" &&
		part.Source.BaseMediaType() != intent.OutputFormat.MediaType() {
		return fmt.Errorf(
			"media type %s does not match requested format %q",
			quoteToken(part.Source.MediaType()),
			intent.OutputFormat,
		)
	}
	return nil
}

func validateGenerateAudio(part message.AudioPart, intent AudioIntent) error {
	if part.Format == nil {
		return fmt.Errorf("audio format is required")
	}
	if part.Format.Encoding != intent.Format.Encoding {
		return fmt.Errorf("audio encoding does not match requested format")
	}
	if intent.Format.SampleRateHz != 0 &&
		part.Format.SampleRateHz != intent.Format.SampleRateHz {
		return fmt.Errorf("audio sample rate does not match requested format")
	}
	if intent.Format.Channels != 0 && part.Format.Channels != intent.Format.Channels {
		return fmt.Errorf("audio channels do not match requested format")
	}
	return nil
}

func validateGenerateText(output string, format *ResponseFormat) error {
	if format == nil || format.Kind == ResponseText {
		return nil
	}
	var value any
	if err := decodeStrict([]byte(output), &value); err != nil {
		return fmt.Errorf("structured generate response is not valid JSON: %w", err)
	}
	if format.Kind == ResponseJSONObject {
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("structured generate response must be a JSON object")
		}
		return nil
	}
	compiler := newInMemoryJSONSchemaCompiler()
	const resource = "inference://generate-response-schema.json"
	if err := compiler.AddResource(resource, bytes.NewReader(format.Schema)); err != nil {
		return fmt.Errorf("load generate response JSON schema: %w", err)
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		return fmt.Errorf("compile generate response JSON schema: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("generate response does not match requested JSON schema: %w", err)
	}
	return nil
}
