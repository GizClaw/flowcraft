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
	if r.Message.Role != message.RoleAssistant {
		return fmt.Errorf("generate response message must have assistant role")
	}
	if err := r.FinishReason.Validate(); err != nil {
		return err
	}
	if len(r.Message.Content.Parts) == 0 {
		if r.FinishReason == FinishCompleted || r.FinishReason == FinishToolCalls {
			return fmt.Errorf("generate response content is required for finish reason %q", r.FinishReason)
		}
	} else if err := r.Message.Validate(); err != nil {
		return err
	}
	if err := r.Usage.Validate(); err != nil {
		return err
	}
	if err := r.ProviderOutputs.Validate(); err != nil {
		return err
	}
	hasToolCalls := r.Message.HasToolCalls()
	if (r.FinishReason == FinishToolCalls) != hasToolCalls {
		return fmt.Errorf("tool-call finish reason does not match response tool calls")
	}
	for _, part := range r.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return err
		}
		switch normalized.(type) {
		case message.TextPart, message.ImagePart, message.AudioPart, message.VideoPart, message.ToolCallPart, message.ReasoningPart:
		default:
			return fmt.Errorf("generate response contains unsupported part %q", normalized.Kind())
		}
	}
	return nil
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
	return fmt.Sprintf("generate tool call %d names undefined tool %q", e.Index, e.Call.Name)
}

func (r GenerateResponse) ValidateFor(request GenerateRequest) error {
	deriveGenerateUsage(request, &r)
	if err := r.Validate(); err != nil {
		return err
	}
	intent := request.Input.Content.Intent
	toolsRequested := intent.Text != nil && intent.Text.toolsRequested()
	requested := make(map[message.PartKind]struct{}, 4)
	for _, kind := range intent.OutputKinds() {
		requested[kind] = struct{}{}
	}
	var text strings.Builder
	textParts := 0
	var images []message.ImagePart
	var audio []message.AudioPart
	var videos []message.VideoPart
	var toolCalls []message.ToolCallPart
	for _, part := range r.Message.Content.Parts {
		normalized, err := message.NormalizePart(part)
		if err != nil {
			return err
		}
		switch value := normalized.(type) {
		case message.TextPart:
			if _, ok := requested[message.PartText]; !ok {
				return fmt.Errorf("generate response contains unrequested text")
			}
			textParts++
			text.WriteString(value.Text)
		case message.ImagePart:
			if _, ok := requested[message.PartImage]; !ok {
				return fmt.Errorf("generate response contains unrequested image")
			}
			images = append(images, value)
			if err := validateGenerateImage(value, *intent.Image); err != nil {
				return fmt.Errorf("generate image %d: %w", len(images)-1, err)
			}
		case message.AudioPart:
			if _, ok := requested[message.PartAudio]; !ok {
				return fmt.Errorf("generate response contains unrequested audio")
			}
			audio = append(audio, value)
			if err := validateGenerateAudio(value, *intent.Audio); err != nil {
				return fmt.Errorf("generate audio %d: %w", len(audio)-1, err)
			}
		case message.VideoPart:
			if _, ok := requested[message.PartVideo]; !ok {
				return fmt.Errorf("generate response contains unrequested video")
			}
			videos = append(videos, value)
			if err := validateGenerateVideo(value); err != nil {
				return fmt.Errorf("generate video %d: %w", len(videos)-1, err)
			}
		case message.ToolCallPart:
			if !toolsRequested {
				return fmt.Errorf("generate response contains an unrequested tool call")
			}
			toolCalls = append(toolCalls, value)
		case message.ReasoningPart:
			// Reasoning is a trace of the model's own process, not a
			// requested artifact: reasoning-capable models emit it whether
			// or not the request set a reasoning intent, so responses may
			// always carry it.
		}
	}
	if toolsRequested {
		definitions := make(map[string]struct{}, len(intent.Text.Tools))
		for _, definition := range intent.Text.Tools {
			definitions[definition.Name] = struct{}{}
		}
		for index, call := range toolCalls {
			if _, ok := definitions[call.Call.Name]; !ok {
				return &undefinedToolError{Index: index, Call: call.Call}
			}
		}
		if choice := intent.Text.ToolChoice; choice != nil {
			switch choice.Kind {
			case ToolChoiceNone:
				if len(toolCalls) != 0 {
					return fmt.Errorf("tool choice none forbids tool calls")
				}
			case ToolChoiceRequired:
				if len(toolCalls) == 0 && r.FinishReason == FinishCompleted {
					return fmt.Errorf("required tool choice produced no tool call")
				}
			case ToolChoiceNamed:
				for _, call := range toolCalls {
					if call.Call.Name != choice.Name {
						return fmt.Errorf(
							"named tool choice %q produced tool %q",
							choice.Name,
							call.Call.Name,
						)
					}
				}
				if len(toolCalls) == 0 && r.FinishReason == FinishCompleted {
					return fmt.Errorf("named tool choice produced no tool call")
				}
			}
		}
	}
	if r.FinishReason != FinishCompleted {
		return nil
	}
	if intent.Text != nil {
		if textParts == 0 {
			return fmt.Errorf("completed generate response contains no requested text")
		}
		if err := validateGenerateText(text.String(), intent.Text.Response); err != nil {
			return err
		}
	}
	if intent.Image != nil {
		if err := validateGenerateCount("image", len(images), intent.Image.Count); err != nil {
			return err
		}
	}
	if intent.Audio != nil {
		if err := validateGenerateCount("audio", len(audio), intent.Audio.Count); err != nil {
			return err
		}
	}
	if intent.Video != nil {
		if err := validateGenerateCount("video", len(videos), nil); err != nil {
			return err
		}
	}
	return nil
}

// validateGenerateVideo checks the output part is genuinely video. Unlike
// image and audio, no canonical video parameter constrains the returned
// encoding beyond the media family.
func validateGenerateVideo(part message.VideoPart) error {
	if mediaType := part.Source.BaseMediaType(); !strings.HasPrefix(mediaType, "video/") {
		return fmt.Errorf("video part media type %q is not video", part.Source.MediaType())
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
			"media type %q does not match requested format %q",
			part.Source.MediaType(),
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
