package dashscope

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/message"
)

// modelKind selects the compiler and transport that serve a model. The kind,
// not the model name, picks the family.
type modelKind string

const (
	kindImage modelKind = "image"
	kindVideo modelKind = "video"
)

// Image protocols.
const (
	// imageAPIAsync is the default: create a task, poll it, then read the
	// generated links from the terminal query response.
	imageAPIAsync = ""
	// imageAPISync is the single-request multimodal-generation call Wan 2.6
	// and later image models serve.
	imageAPISync = "sync"
)

// Video input shapes. Each selects both the endpoint and the way input
// images lower onto it.
const (
	videoInputText      = "t2v"
	videoInputImage     = "i2v"
	videoInputKeyframes = "kf2v"
)

// Video resolution parameters.
const (
	videoSizeResolution = "resolution"
	videoSizeConcrete   = "size"
)

// validateModel enforces the family contract: the compiler bound by kind can
// only serve the output modalities it produces, so kind and capabilities
// cannot drift.
func validateModel(declared ModelSpec) error {
	capabilities := declared.Capabilities
	switch modelKind(declared.Kind) {
	case kindImage:
		if !slices.Contains(capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindVideo:
		if !slices.Contains(capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
	default:
		return fmt.Errorf("unsupported kind %q", declared.Kind)
	}
	if capabilities.CustomEmbedDimensions {
		return fmt.Errorf(
			"this driver has no embed family; custom_embed_dimensions would never compile",
		)
	}
	if capabilities.HostedWebSearch {
		return fmt.Errorf(
			"the Wan media surfaces have no server-side tools; hosted_web_search would never compile",
		)
	}
	return declared.Limits.Validate()
}

// wireModel returns the model token sent on the wire: the declared name
// unless the declaration aliases it to another model.
func wireModel(name string, declared ModelSpec) string {
	if declared.WireModel != "" {
		return declared.WireModel
	}
	return name
}
