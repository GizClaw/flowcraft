package minimax

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/message"
)

// modelKind selects the compiler and transport that serve a model. The kind,
// not the model name, picks the family.
type modelKind string

const (
	kindImage     modelKind = "image"
	kindTTS       modelKind = "tts"
	kindVideo     modelKind = "video"
	kindMusic     modelKind = "music"
	kindContextIR modelKind = "context_ir"
)

// videoAPIV2 is the v2 video task API (MiniMax-H3): a multimodal content
// array, 768P/2K tiers, 4-15s durations, and ratio control. The empty value
// is the flat v1 API the Hailuo 2.x/02 trio rides.
const videoAPIV2 = "v2"

// VideoParams declares the video-surface facts for one kind "video" model:
// which task API it speaks and which parameters that API accepts for it.
// Zero values mean undeclared, so an undeclared flag compiles with
// syntax-only validation and the endpoint's own validation decides.
type VideoParams struct {
	// API selects the task API: "" (v1) or "v2".
	API string `json:"api,omitempty"`
	// TenSeconds accepts 10-second durations at 768P.
	TenSeconds bool `json:"ten_seconds,omitempty"`
	// HD accepts 1080P resolution.
	HD bool `json:"hd,omitempty"`
	// P512 accepts 512P resolution; MiniMax-Hailuo-02 serves it on the
	// image-to-video surface only.
	P512 bool `json:"p512,omitempty"`
	// LastFrame accepts a second input image as the closing frame
	// (first_frame_image + last_frame_image); MiniMax-Hailuo-02 only.
	LastFrame bool `json:"last_frame,omitempty"`
	// ImageToVideoOnly marks image-to-video models: the request must carry a
	// first-frame image.
	ImageToVideoOnly bool `json:"image_to_video_only,omitempty"`
}

// videoV2 reports whether the model rides the v2 task API.
func (m ModelSpec) videoV2() bool { return m.Video.API == videoAPIV2 }

// validateModel enforces the family contract: the compiler bound by kind can
// only serve the output modalities it produces, so kind and capabilities
// cannot drift.
func validateModel(declared ModelSpec) error {
	capabilities := declared.Capabilities
	if err := capabilities.Validate(); err != nil {
		return err
	}
	switch modelKind(declared.Kind) {
	case kindImage:
		if !slices.Contains(capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindTTS, kindMusic:
		if !slices.Contains(capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("%s family must declare audio output", declared.Kind)
		}
	case kindVideo:
		if !slices.Contains(capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
	case kindContextIR:
		if !slices.Contains(capabilities.Outputs, message.PartText) {
			return fmt.Errorf("context_ir family must declare text output")
		}
	default:
		return fmt.Errorf("unsupported kind %q", declared.Kind)
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
