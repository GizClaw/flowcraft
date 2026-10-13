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
	// kindGenerate is the text-generation family: the native text surfaces
	// (chat-style generation with tools, structured output, reasoning, and
	// server-side search).
	kindGenerate modelKind = "generate"
	// kindEmbed is the text-embedding family.
	kindEmbed modelKind = "embed"
	// kindTTS is the speech-synthesis family: the Qwen-TTS models on the
	// multimodal-generation surface, which answer with one WAV file.
	kindTTS modelKind = "tts"
	// kindTranscribe is the file-transcription family: the speech-recognition
	// models on the async task pipeline, which submit one audio file URL and
	// answer with a transcript once the task reaches a terminal state.
	kindTranscribe modelKind = "transcribe"
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

// Text-generation protocol surfaces, declared per model.
const (
	// generateAPIText is the default: the classic text-generation surface,
	// whose message content is a plain string and whose streaming rides the
	// SSE header alone.
	generateAPIText = ""
	// generateAPIMultimodal is the multimodal-generation surface, which the
	// Qwen3.5+ commercial families and the VL/Omni models require even for
	// text input: message content is an array of typed entries and a
	// streamed call adds parameters.stream.
	generateAPIMultimodal = "multimodal"
)

// The wire parameters the canonical max_output_tokens control lowers to.
const (
	// maxTokensParamAnswer is the default: parameters.max_tokens limits the
	// answer and, on thinking models, excludes the trace.
	maxTokensParamAnswer = ""
	// maxTokensParamMaxTokens spells that legacy parameter explicitly.
	maxTokensParamMaxTokens = "max_tokens"
	// maxTokensParamCompletion is parameters.max_completion_tokens, which
	// limits answer plus trace; the platform recommends it for thinking
	// models.
	maxTokensParamCompletion = "max_completion_tokens"
)

// validateModel enforces the family contract: the compiler bound by kind can
// only serve the output modalities it produces, so kind and capabilities
// cannot drift.
func validateModel(declared ModelSpec) error {
	capabilities := declared.Capabilities
	kind := modelKind(declared.Kind)
	switch kind {
	case kindImage:
		if !slices.Contains(capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindVideo:
		if !slices.Contains(capabilities.Outputs, message.PartVideo) {
			return fmt.Errorf("video family must declare video output")
		}
	case kindGenerate:
		if !slices.Contains(capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
		for _, output := range capabilities.Outputs {
			if output != message.PartText {
				return fmt.Errorf(
					"the generate family publishes text output only; %q output would never compile",
					output,
				)
			}
		}
		if declared.Generate.API != generateAPIMultimodal {
			for _, input := range capabilities.Inputs {
				switch input {
				case message.PartImage, message.PartVideo, message.PartAudio:
					return fmt.Errorf(
						"model declares %q input but rides the text-generation surface, which carries no media content; declare generate.api: multimodal",
						input,
					)
				}
			}
		}
		if len(capabilities.Reasoning.EffortMap) > 0 {
			return fmt.Errorf(
				"reasoning.effort_map would never compile: the DashScope thinking dial is a token budget (generate_options.thinking_budget), not a level",
			)
		}
	case kindEmbed:
		if len(capabilities.Outputs) > 0 {
			return fmt.Errorf("the embed family declares no generate output")
		}
		if capabilities.HostedWebSearch {
			return fmt.Errorf(
				"the embed surface has no server-side tools; hosted_web_search would never compile",
			)
		}
		if capabilities.Reasoning.Kind != "" {
			return fmt.Errorf("the embed surface has no reasoning channel")
		}
		for _, input := range capabilities.Inputs {
			if input != message.PartText {
				return fmt.Errorf(
					"the text-embedding surface embeds text; %q input would never compile",
					input,
				)
			}
		}
	case kindTTS:
		if !slices.Contains(capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("the speech family must declare audio output")
		}
		for _, output := range capabilities.Outputs {
			if output != message.PartAudio {
				return fmt.Errorf(
					"the speech family publishes audio output only; %q output would never compile",
					output,
				)
			}
		}
		if capabilities.HostedWebSearch {
			return fmt.Errorf(
				"the speech surface has no server-side tools; hosted_web_search would never compile",
			)
		}
		if capabilities.Reasoning.Kind != "" || len(capabilities.Reasoning.EffortMap) > 0 {
			return fmt.Errorf("the speech surface has no reasoning channel")
		}
		for _, input := range capabilities.Inputs {
			if input != message.PartText {
				return fmt.Errorf(
					"the speech surface speaks text; %q input would never compile",
					input,
				)
			}
		}
		if declared.TTS.Encoding == "" {
			return fmt.Errorf(
				"the speech surface has no format control, so the declaration has to state the encoding it serves; set tts.encoding (pcm16 on the Qwen-TTS family)",
			)
		}
	case kindTranscribe:
		// The transcription compiler reads one audio file and publishes
		// text, so a declaration that names any other modality would never
		// compile.
		for _, input := range capabilities.Inputs {
			if input != message.PartAudio {
				return fmt.Errorf(
					"the file-transcription surface reads one audio file; %q input would never compile",
					input,
				)
			}
		}
		for _, output := range capabilities.Outputs {
			if output != message.PartText {
				return fmt.Errorf(
					"the file-transcription surface publishes text; %q output would never compile",
					output,
				)
			}
		}
		if capabilities.Reasoning.Kind != "" ||
			len(capabilities.Reasoning.EffortMap) > 0 {
			return fmt.Errorf(
				"the file-transcription surface has no reasoning channel")
		}
	default:
		return fmt.Errorf(
			"unsupported kind %q; this driver serves the Wan media families (kind \"image\" or \"video\"), native text generation (kind \"generate\"), text embeddings (kind \"embed\"), native speech synthesis (kind \"tts\"), and native file transcription (kind \"transcribe\")",
			declared.Kind,
		)
	}
	if kind != kindEmbed && capabilities.CustomEmbedDimensions {
		return fmt.Errorf(
			"custom_embed_dimensions is an embed-family fact; it would never compile on kind %q",
			kind,
		)
	}
	if kind != kindGenerate && capabilities.HostedWebSearch {
		return fmt.Errorf(
			"the media, speech, and embed surfaces have no server-side tools; hosted_web_search would never compile",
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
