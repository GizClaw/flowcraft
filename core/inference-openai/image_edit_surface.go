package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
)

// imageEditCompile maps an edit request: a prompt plus one or more reference
// images becomes the multipart edit call. The surface is chosen by the caller's
// content, not by a flag: a request carrying an image is an edit.
//
// Multipart bodies are not replayable, so the SDK declines to retry them; the
// deployment's retry setting therefore has no effect on this endpoint. That is an
// HTTP fact rather than a defect, and it is why no retry loop is layered on top.
func (d *driver) imageEditCompile(
	_ context.Context,
	request inference.GenerateRequest,
	shape protocol.Shape,
) (protocol.Compiled[openai.ImageEditParams], error) {
	if request.Output.Kind() != inference.OutputImage {
		return protocol.Compiled[openai.ImageEditParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldOutput,
			fmt.Errorf("openai: image edit pipeline cannot serve output %q", request.Output.Kind()),
		)
	}
	if err := d.checkDeclarations(request); err != nil {
		return protocol.Compiled[openai.ImageEditParams]{}, err
	}
	if len(request.Context) > 0 {
		return protocol.Compiled[openai.ImageEditParams]{}, inference.NewError(
			inference.UnsupportedFeature,
			inference.FieldContext,
			fmt.Errorf("openai: image editing has no conversation context; put the prompt in the input"),
		)
	}
	options, err := imageOptions(request.Extensions)
	if err != nil {
		return protocol.Compiled[openai.ImageEditParams]{}, err
	}
	prompt, images, err := editInput(request.Input.Message())
	if err != nil {
		return protocol.Compiled[openai.ImageEditParams]{}, err
	}

	params := openai.ImageEditParams{
		Model:  openai.ImageModel(request.Model.ID.Name),
		Prompt: prompt,
		Image:  editImageUnion(images),
	}
	if err := editApply(&params, options); err != nil {
		return protocol.Compiled[openai.ImageEditParams]{}, err
	}
	if shape == protocol.ShapeStream && options != nil && options.PartialImages != nil {
		params.PartialImages = openai.Int(int64(*options.PartialImages))
	}

	fields := append(honoredFields(request), qualifiedFields(options)...)
	return protocol.Compiled[openai.ImageEditParams]{
		Wire:   params,
		Fields: fields,
	}, nil
}

// editInput splits the turn into a prompt and the reference images. At least one
// image and a non-empty prompt are required by the endpoint.
func editInput(item message.Message) (string, []imageFile, error) {
	texts := make([]string, 0, len(item.Content.Parts))
	images := make([]imageFile, 0, len(item.Content.Parts))
	for _, part := range item.Content.Parts {
		switch typed := part.(type) {
		case message.TextPart:
			texts = append(texts, typed.Text)
		case message.ImagePart:
			data, mediaType, err := inlineImageBytes(typed)
			if err != nil {
				return "", nil, err
			}
			images = append(images, imageFile{data: data, mediaType: mediaType})
		default:
			return "", nil, inference.NewError(
				inference.UnsupportedFeature,
				inference.FieldContent,
				fmt.Errorf("openai: image editing accepts text and images, not %q", part.Kind()),
			)
		}
	}
	if len(images) == 0 {
		return "", nil, inference.NewError(
			inference.InvalidRequest,
			inference.FieldContent,
			fmt.Errorf("openai: image editing requires at least one reference image"),
		)
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n"))
	if prompt == "" {
		return "", nil, inference.NewError(
			inference.InvalidRequest,
			inference.FieldContent,
			fmt.Errorf("openai: image editing requires a prompt"),
		)
	}
	return prompt, images, nil
}

// imageFile is one uploadable image: its bytes, and the media type the wire must
// declare. The edit endpoint rejects a part announced as
// application/octet-stream, so the SDK has to be told what the bytes are.
type imageFile struct {
	data      []byte
	mediaType string
}

// reader returns a reader that carries the media type and a plausible filename.
func (f imageFile) reader() io.Reader {
	return mediaReader{Reader: bytes.NewReader(f.data), mediaType: f.mediaType}
}

// mediaReader satisfies the two interfaces the SDK's multipart encoder looks for,
// which is how a part gets a real Content-Type instead of a generic one.
type mediaReader struct {
	io.Reader
	mediaType string
}

func (r mediaReader) ContentType() string { return r.mediaType }

func (r mediaReader) Filename() string {
	switch r.mediaType {
	case "image/jpeg":
		return "image.jpg"
	case "image/webp":
		return "image.webp"
	default:
		return "image.png"
	}
}

// editImageUnion builds the multipart image field: one file, or several.
func editImageUnion(images []imageFile) openai.ImageEditParamsImageUnion {
	readers := make([]io.Reader, len(images))
	for i, file := range images {
		readers[i] = file.reader()
	}
	if len(readers) == 1 {
		return openai.ImageEditParamsImageUnion{OfFile: readers[0]}
	}
	return openai.ImageEditParamsImageUnion{OfFileArray: readers}
}

// editApply maps the shared image options onto the edit parameters. A mask that
// cannot be expressed is an error, never a skipped field.
func editApply(params *openai.ImageEditParams, options *ImageOptions) error {
	if options == nil {
		return nil
	}
	if options.Count != nil {
		params.N = openai.Int(int64(*options.Count))
	}
	if options.Size != "" {
		params.Size = openai.ImageEditParamsSize(options.Size)
	}
	if options.Quality != "" {
		params.Quality = openai.ImageEditParamsQuality(options.Quality)
	}
	if options.OutputFormat != "" {
		params.OutputFormat = openai.ImageEditParamsOutputFormat(options.OutputFormat)
	}
	if options.ResponseFormat != "" {
		params.ResponseFormat = openai.ImageEditParamsResponseFormat(options.ResponseFormat)
	}
	if options.Background != "" {
		params.Background = openai.ImageEditParamsBackground(options.Background)
	}
	if options.OutputCompression != nil {
		params.OutputCompression = openai.Int(int64(*options.OutputCompression))
	}
	if options.SafetyIdentifier != "" {
		return unsupportedOption("safety_identifier", "the image surfaces (not wired yet)")
	}
	if options.InputFidelity != "" {
		params.InputFidelity = openai.ImageEditParamsInputFidelity(options.InputFidelity)
	}
	if options.Mask != nil {
		data, mediaType, err := inlineImageBytes(message.ImagePart{Source: *options.Mask})
		if err != nil {
			return err
		}
		params.Mask = imageFile{data: data, mediaType: mediaType}.reader()
	}
	return nil
}

// imageEditCall performs the provider I/O.
func (d *driver) imageEditCall(
	ctx context.Context,
	_ inference.GenerateRequest,
	params openai.ImageEditParams,
) (*openai.ImagesResponse, error) {
	return d.client.Images.Edit(ctx, params)
}
