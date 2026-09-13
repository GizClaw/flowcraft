package bytedance

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/message"
)

// modelKind groups models by the service family that serves them. The kind,
// not the model name, selects the compiler and transport.
type modelKind string

const (
	kindGenerate modelKind = "generate"
	kindEmbed    modelKind = "embed"
	kindImage    modelKind = "image"
	kindVideo    modelKind = "video"
)

// validateModel enforces the family contract: the compiler bound by kind can
// only serve the output modalities it produces, so kind and capabilities
// cannot drift, and a declared video control fact has to be coherent with the
// inputs the model accepts.
func validateModel(declared ModelSpec) error {
	capabilities := declared.Capabilities
	if err := capabilities.Validate(); err != nil {
		return err
	}
	switch modelKind(declared.Kind) {
	case kindGenerate:
		if !slices.Contains(capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
	case kindImage:
		if !slices.Contains(capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindVideo:
		if !slices.Contains(capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
		if declared.Video.ReferenceImage > 0 &&
			!slices.Contains(capabilities.Inputs, message.PartImage) {
			return fmt.Errorf(
				"video family declaring reference images must accept image input")
		}
		if declared.Video.ReferenceVideo > 0 &&
			!slices.Contains(capabilities.Inputs, message.PartVideo) {
			return fmt.Errorf(
				"video family declaring reference videos must accept video input")
		}
		if declared.Video.ReferenceAudio > 0 &&
			!slices.Contains(capabilities.Inputs, message.PartAudio) {
			return fmt.Errorf(
				"video family declaring reference audio must accept audio input")
		}
	case kindEmbed:
		if len(capabilities.Outputs) != 0 {
			return fmt.Errorf("embed family declares no generate output")
		}
	}
	return declared.Limits.Validate()
}
