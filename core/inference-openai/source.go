package openai

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// This file holds the leaf conversions both wire dialects share. Everything
// structural — how a conversation becomes messages, how a tool result becomes an
// item — is written twice on purpose: the two dialects model messages
// differently, and a shared intermediate structure would be a second message
// model to keep in sync with the two real ones.

// imageURL renders an image source as the URL the wire expects: a remote
// reference as-is, inline bytes as a data URL.
func imageURL(source media.ImageSource) (string, error) {
	if err := source.Validate(); err != nil {
		return "", inference.NewError(
			inference.InvalidRequest, inference.OperationGenerate, inference.FieldContent, err,
		)
	}
	switch source.Kind() {
	case media.SourceURL:
		return source.URL(), nil
	default:
		return dataURL(source.MediaType(), source.Bytes()), nil
	}
}

// audioPayload renders an audio source as base64 data plus the wire format
// token. The chat surface accepts inline audio only, so a URL source is
// rejected rather than fetched here.
func audioPayload(source media.AudioSource) (string, string, error) {
	if err := source.Validate(); err != nil {
		return "", "", inference.NewError(
			inference.InvalidRequest, inference.OperationGenerate, inference.FieldContent, err,
		)
	}
	if source.Kind() != media.SourceInline {
		return "", "", inference.NewError(
			inference.UnsupportedFeature, inference.OperationGenerate, inference.FieldContent,
			fmt.Errorf("openai: audio input must be inline, got a %q source", source.Kind()),
		)
	}
	format, err := audioFormat(source.MediaType())
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(source.Bytes()), format, nil
}

// audioFormat maps a media type onto the wire's format token. The wire accepts a
// closed set, so anything else is rejected instead of being sent and refused.
func audioFormat(mediaType string) (string, error) {
	switch strings.ToLower(mediaType) {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav", nil
	case "audio/mpeg", "audio/mp3":
		return "mp3", nil
	default:
		return "", inference.NewError(
			inference.UnsupportedFeature, inference.OperationGenerate, inference.FieldContent,
			fmt.Errorf("openai: audio media type %q is not a supported input format", mediaType),
		)
	}
}

// dataURL encodes inline bytes as a media data URL.
func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// textOf concatenates the text parts of a message. It is what the wire takes
// where the field is a plain string rather than a content array.
func textOf(item message.Message) string { return item.Content.Text() }

// unsupportedPart builds the rejection used when a content kind has no mapping.
func unsupportedPart(kind message.PartKind) error {
	return inference.NewError(
		inference.UnsupportedFeature,
		inference.OperationGenerate,
		inference.FieldContent,
		fmt.Errorf("openai: content part %q is not mapped", kind),
	)
}
