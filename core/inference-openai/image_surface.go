package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// ImageExtensionID names the image-generation options extension. It is separate
// from [GenerationExtensionID] because the two option sets do not intersect: one
// struct holding both would put text sampling next to image sizing.
const ImageExtensionID = "image"

// ImageOptions configures one image generation call.
type ImageOptions struct {
	// Count is how many images to produce ("n"). Nil means the provider default.
	Count *int `json:"count,omitempty" yaml:"count,omitempty"`
	// Size is the output dimensions, for example "1024x1024", or "auto".
	Size string `json:"size,omitempty" yaml:"size,omitempty"`
	// Quality is "low", "medium", "high" or "auto".
	Quality string `json:"quality,omitempty" yaml:"quality,omitempty"`
	// Style is "vivid" or "natural" (DALL·E only).
	Style string `json:"style,omitempty" yaml:"style,omitempty"`
	// OutputFormat is the encoded image format: "png", "jpeg" or "webp".
	OutputFormat string `json:"output_format,omitempty" yaml:"output_format,omitempty"`
	// ResponseFormat asks for "b64_json" (default) or "url".
	ResponseFormat string `json:"response_format,omitempty" yaml:"response_format,omitempty"`
	// Background is "transparent", "opaque" or "auto".
	Background string `json:"background,omitempty" yaml:"background,omitempty"`
	// Moderation is "low" or "auto".
	Moderation string `json:"moderation,omitempty" yaml:"moderation,omitempty"`
	// OutputCompression is the 0-100 compression level for lossy formats.
	OutputCompression *int `json:"output_compression,omitempty" yaml:"output_compression,omitempty"`
	// SafetyIdentifier is a stable, privacy-preserving end-user identifier.
	SafetyIdentifier string `json:"safety_identifier,omitempty" yaml:"safety_identifier,omitempty"`
	// Mask is the region to repaint (edits only). Only an inline source is
	// accepted: a URL would have to be fetched, and compilation never does I/O.
	Mask *media.ImageSource `json:"mask,omitempty" yaml:"mask,omitempty"`
	// InputFidelity is "low" or "high" (edits only).
	InputFidelity string `json:"input_fidelity,omitempty" yaml:"input_fidelity,omitempty"`
	// PartialImages is how many interim images a streamed generation emits. Zero
	// emits only the finished one.
	PartialImages *int `json:"partial_images,omitempty" yaml:"partial_images,omitempty"`
}

// ProviderID implements [inference.Extension].
func (*ImageOptions) ProviderID() string { return ProviderID }

// ExtensionID implements [inference.Extension].
func (*ImageOptions) ExtensionID() string { return ImageExtensionID }

// ActiveFields lists the options this value actually sets.
func (o *ImageOptions) ActiveFields() []inference.ExtensionField {
	if o == nil {
		return nil
	}
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.Count != nil {
		add("count")
	}
	if o.Size != "" {
		add("size")
	}
	if o.Quality != "" {
		add("quality")
	}
	if o.Style != "" {
		add("style")
	}
	if o.OutputFormat != "" {
		add("output_format")
	}
	if o.ResponseFormat != "" {
		add("response_format")
	}
	if o.Background != "" {
		add("background")
	}
	if o.Moderation != "" {
		add("moderation")
	}
	if o.OutputCompression != nil {
		add("output_compression")
	}
	if o.SafetyIdentifier != "" {
		add("safety_identifier")
	}
	if o.Mask != nil {
		add("mask")
	}
	if o.InputFidelity != "" {
		add("input_fidelity")
	}
	if o.PartialImages != nil {
		add("partial_images")
	}
	return fields
}

// Validate checks the options' own invariants. Value sets the provider defines are
// passed through: an unsupported value is a provider rejection, not a silent
// substitution, and mirroring every model's accepted list here would rot.
func (o *ImageOptions) Validate() error {
	if o == nil {
		return nil
	}
	if o.Count != nil && *o.Count <= 0 {
		return fmt.Errorf("count must be positive")
	}
	if o.OutputCompression != nil && (*o.OutputCompression < 0 || *o.OutputCompression > 100) {
		return fmt.Errorf("output_compression must be within [0, 100]")
	}
	switch o.ResponseFormat {
	case "", "b64_json", "url":
	default:
		return fmt.Errorf("unknown response_format %q", o.ResponseFormat)
	}
	switch o.OutputFormat {
	case "", "png", "jpeg", "webp":
	default:
		return fmt.Errorf("unknown output_format %q", o.OutputFormat)
	}
	switch o.InputFidelity {
	case "", "low", "high":
	default:
		return fmt.Errorf("unknown input_fidelity %q", o.InputFidelity)
	}
	if o.PartialImages != nil && (*o.PartialImages < 0 || *o.PartialImages > 3) {
		return fmt.Errorf("partial_images must be within [0, 3]")
	}
	return nil
}

// Clone returns an independent copy.
func (o *ImageOptions) Clone() inference.Extension {
	if o == nil {
		return (*ImageOptions)(nil)
	}
	clone := *o
	if o.Count != nil {
		value := *o.Count
		clone.Count = &value
	}
	if o.OutputCompression != nil {
		value := *o.OutputCompression
		clone.OutputCompression = &value
	}
	if o.PartialImages != nil {
		value := *o.PartialImages
		clone.PartialImages = &value
	}
	if o.Mask != nil {
		mask := o.Mask.Clone()
		clone.Mask = &mask
	}
	return &clone
}

// decodeImage is the config-side decoder for the image options extension.
func decodeImage(fields json.RawMessage) (inference.Extension, error) {
	options := &ImageOptions{}
	if len(fields) > 0 {
		if err := inference.DecodeStrict(fields, options); err != nil {
			return nil, err
		}
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return options, nil
}

// imageOptions extracts this driver's image extension from a request.
func imageOptions(extensions inference.Extensions) (*ImageOptions, error) {
	var found *ImageOptions
	for _, extension := range extensions {
		options, ok := extension.(*ImageOptions)
		if !ok {
			continue
		}
		if found != nil {
			return nil, inference.NewError(
				inference.InvalidExtension,
				inference.FieldExtensions,
				fmt.Errorf("openai: duplicate image options"),
			)
		}
		found = options
	}
	return found, nil
}

// imageCompile maps an image request onto the image generation parameters.
func (d *driver) imageCompile(
	_ context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[openai.ImageGenerateParams], error) {
	if request.Output.Kind() != inference.OutputImage {
		return protocol.Compiled[openai.ImageGenerateParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldOutput,
			fmt.Errorf("openai: image pipeline cannot serve output %q", request.Output.Kind()),
		)
	}
	if err := d.checkDeclarations(request); err != nil {
		return protocol.Compiled[openai.ImageGenerateParams]{}, err
	}
	if len(request.Context) > 0 {
		return protocol.Compiled[openai.ImageGenerateParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldContext,
			fmt.Errorf("openai: image generation has no conversation context; put the prompt in the input"),
		)
	}
	options, err := imageOptions(request.Extensions)
	if err != nil {
		return protocol.Compiled[openai.ImageGenerateParams]{}, err
	}
	prompt, err := imagePrompt(request.Input.Message())
	if err != nil {
		return protocol.Compiled[openai.ImageGenerateParams]{}, err
	}
	params := openai.ImageGenerateParams{
		Model:  openai.ImageModel(request.Model.ID.Name),
		Prompt: prompt,
	}
	if err := imageApply(&params, options); err != nil {
		return protocol.Compiled[openai.ImageGenerateParams]{}, err
	}
	if shape == protocol.ShapeStream {
		if options != nil && options.Count != nil && *options.Count > 1 {
			return protocol.Compiled[openai.ImageGenerateParams]{}, inference.NewError(
				inference.UnsupportedFeature,
				inference.FieldStream,
				fmt.Errorf("openai: a streamed generation produces exactly one image"),
			)
		}
		if options != nil && options.PartialImages != nil {
			params.PartialImages = openai.Int(int64(*options.PartialImages))
		}
	}

	fields := append(honoredFields(request), qualifiedFields(options)...)
	return protocol.Compiled[openai.ImageGenerateParams]{
		Wire:   params,
		Fields: fields,
	}, nil
}

// imageApply maps the options onto the wire parameters.
func imageApply(params *openai.ImageGenerateParams, options *ImageOptions) error {
	if options == nil {
		return nil
	}
	if options.Count != nil {
		params.N = openai.Int(int64(*options.Count))
	}
	if options.Size != "" {
		params.Size = openai.ImageGenerateParamsSize(options.Size)
	}
	if options.Quality != "" {
		params.Quality = openai.ImageGenerateParamsQuality(options.Quality)
	}
	if options.Style != "" {
		params.Style = openai.ImageGenerateParamsStyle(options.Style)
	}
	if options.OutputFormat != "" {
		params.OutputFormat = openai.ImageGenerateParamsOutputFormat(options.OutputFormat)
	}
	if options.ResponseFormat != "" {
		params.ResponseFormat = openai.ImageGenerateParamsResponseFormat(options.ResponseFormat)
	}
	if options.Background != "" {
		params.Background = openai.ImageGenerateParamsBackground(options.Background)
	}
	if options.Moderation != "" {
		params.Moderation = openai.ImageGenerateParamsModeration(options.Moderation)
	}
	if options.OutputCompression != nil {
		params.OutputCompression = openai.Int(int64(*options.OutputCompression))
	}
	if options.SafetyIdentifier != "" {
		return unsupportedOption("safety_identifier", "the image surfaces (not wired yet)")
	}
	return nil
}

// imagePrompt joins the input's text into the prompt. The image endpoints accept a
// prompt, not a conversation, so any other content kind is an error pointing at the
// surface that does accept it.
func imagePrompt(item message.Message) (string, error) {
	texts := make([]string, 0, len(item.Content.Parts))
	for _, part := range item.Content.Parts {
		text, ok := part.(message.TextPart)
		if !ok {
			return "", inference.NewError(
				inference.UnsupportedFeature,
				inference.FieldContent,
				fmt.Errorf("openai: image generation takes text input only; %q belongs to the edit surface", part.Kind()),
			)
		}
		texts = append(texts, text.Text)
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n"))
	if prompt == "" {
		return "", inference.NewError(
			inference.InvalidRequest,
			inference.FieldContent,
			fmt.Errorf("openai: image generation requires a prompt"),
		)
	}
	return prompt, nil
}

// imageCall performs the provider I/O.
func (d *driver) imageCall(
	ctx context.Context,
	request inference.GenerateRequest,
	params openai.ImageGenerateParams,
) (*openai.ImagesResponse, error) {
	auth, err := d.authOptions(request.Model.Profile)
	if err != nil {
		return nil, err
	}
	return d.client.Images.Generate(ctx, params, auth...)
}

// imageDecode maps the generated images onto content parts. The provider's revised
// prompts have no single-image home, so they ride beside the images as one
// structured data part instead of being dropped.
func (d *driver) imageDecode(
	_ context.Context,
	request inference.GenerateRequest,
	raw *openai.ImagesResponse,
) (inference.GenerateResponse, error) {
	if raw == nil {
		return inference.GenerateResponse{}, fmt.Errorf("openai: nil image response")
	}
	if len(raw.Data) == 0 {
		return inference.GenerateResponse{}, inference.NewError(
			inference.ProviderResponseUnusable,
			"",
			fmt.Errorf("openai: image response carried no image"),
		)
	}
	options, err := imageOptions(request.Extensions)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	mediaType := imageMediaType(options)

	parts := make([]message.Part, 0, len(raw.Data)+1)
	revised := make([]revisedPrompt, 0, len(raw.Data))
	ephemeral := false
	for index, item := range raw.Data {
		part, fromURL, err := imagePart(item, mediaType)
		if err != nil {
			return inference.GenerateResponse{}, err
		}
		if fromURL {
			ephemeral = true
		}
		parts = append(parts, part)
		if item.RevisedPrompt != "" {
			revised = append(revised, revisedPrompt{Index: index, Prompt: item.RevisedPrompt})
		}
	}
	if len(revised) > 0 {
		value, err := json.Marshal(map[string]any{"revised_prompts": revised})
		if err != nil {
			return inference.GenerateResponse{}, fmt.Errorf("openai: revised prompts: %w", err)
		}
		parts = append(parts, message.DataPart{
			MediaType: "application/json",
			Value:     value,
		})
	}

	warnings := make([]inference.Warning, 0, 1)
	if ephemeral {
		warnings = append(warnings, inference.Warning{
			Field:   inference.FieldContent,
			Code:    "ephemeral_url",
			Message: "the provider returned image URLs, which expire; fetch and store them before relying on them",
		})
	}

	count := int64(len(raw.Data))
	return inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.Content{Parts: parts},
		},
		FinishReason: inference.FinishStop,
		Usage: usage.Usage{
			InputTokens:     raw.Usage.InputTokens,
			OutputTokens:    raw.Usage.OutputTokens,
			TotalTokens:     raw.Usage.TotalTokens,
			GeneratedImages: &count,
		},
		Metadata: inference.Metadata{
			Model: model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
		},
		Warnings: warnings,
	}, nil
}

// revisedPrompt records which generated image a revised prompt belongs to.
type revisedPrompt struct {
	Index  int    `json:"index"`
	Prompt string `json:"prompt"`
}

// imagePart converts one generated image. It prefers inline bytes and falls back to
// the provider's URL, reporting which one it used.
func imagePart(item openai.Image, mediaType string) (message.Part, bool, error) {
	if item.B64JSON != "" {
		data, err := base64.StdEncoding.DecodeString(item.B64JSON)
		if err != nil {
			return nil, false, inference.NewError(
				inference.InvalidProviderResponse,
				"",
				fmt.Errorf("openai: image payload is not base64: %w", err),
			)
		}
		source, err := media.NewImageBytes(data, mediaType)
		if err != nil {
			return nil, false, err
		}
		return message.ImagePart{Source: source}, false, nil
	}
	if item.URL != "" {
		source, err := media.NewImageURL(item.URL, mediaType)
		if err != nil {
			return nil, false, err
		}
		return message.ImagePart{Source: source}, true, nil
	}
	return nil, false, inference.NewError(
		inference.InvalidProviderResponse,
		"",
		fmt.Errorf("openai: generated image carries neither bytes nor a URL"),
	)
}

// imageMediaType is the media type the decoded images are labelled with.
func imageMediaType(options *ImageOptions) string {
	format := "png"
	if options != nil && options.OutputFormat != "" {
		format = options.OutputFormat
	}
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

// hasImageInput reports whether the turn carries an image, which selects the edit
// surface. It reads only the input: context is refused for image requests, so
// nothing else can contribute a reference image.
func hasImageInput(request inference.GenerateRequest) bool {
	for _, part := range request.Input.Content.Parts {
		if _, ok := part.(message.ImagePart); ok {
			return true
		}
	}
	return false
}

// inlineImageBytes returns the part's bytes and media type. A URL or stream
// source is refused: fetching or draining it would be I/O, and compilation
// performs none.
func inlineImageBytes(part message.ImagePart) ([]byte, string, error) {
	source := part.Source
	if err := source.Validate(); err != nil {
		return nil, "", inference.NewError(
			inference.InvalidRequest, inference.FieldContent, err,
		)
	}
	if source.Kind() != media.SourceInline {
		return nil, "", inference.NewError(
			inference.UnsupportedFeature, inference.FieldContent,
			fmt.Errorf("openai: editing requires inline image bytes; materialize a %q source first", source.Kind()),
		)
	}
	return source.Bytes(), source.MediaType(), nil
}

// b64ImagePart decodes a base64 image payload into a content part.
func b64ImagePart(payload, mediaType string) (message.Part, error) {
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, inference.NewError(
			inference.InvalidProviderResponse,
			"",
			fmt.Errorf("openai: image payload is not base64: %w", err),
		)
	}
	source, err := media.NewImageBytes(data, mediaType)
	if err != nil {
		return nil, err
	}
	return message.ImagePart{Source: source}, nil
}
