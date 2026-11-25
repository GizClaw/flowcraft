package model

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
)

// ModelCapabilities describes optional feature bits and content kinds a model
// can serve. Zero is the conservative declaration: every feature the struct
// omits is treated as unsupported until a provider declares it.
type ModelCapabilities struct {
	// Inputs lists the canonical content part kinds the model accepts as
	// request input (text, image, audio, video, tool calls, ...). Providers
	// that do not declare inputs leave the capability unknown rather than
	// asserted absent: an empty list filters nothing, while a declared list
	// rejects a request carrying an undeclared part before any driver opens,
	// and the compiler remains the final arbiter for everything the
	// declaration does not cover (the baseline kinds, and structural parts
	// whose role rules are per surface).
	Inputs []message.PartKind `json:"inputs,omitempty"`
	// Modes lists what this model can be asked for: the shapes of answer it
	// produces (text, image, audio, video, tool calls) and the operations that are
	// not a conversation (embed, decide). It is one set because a caller's question
	// is one question — "can this model do X" — and two sets would let a model claim
	// a mode it cannot serve.
	//
	// Empty is not "nothing": see [ModelCapabilities.EffectiveModes], which is what
	// every reader should use.
	Modes []Mode `json:"modes,omitempty"`
	// EmbeddingDimensions is the default vector width this model produces.
	// Zero means the model does not serve embeddings; a positive value is the
	// width a request gets when it asks for none. It is a fact, not a toggle:
	// callers compare it against what they need to store.
	EmbeddingDimensions int `json:"embedding_dimensions,omitempty"`
	// HostedWebSearch marks provider-side web_search tool support. It is
	// discovery metadata for hosts; the search configuration itself still
	// rides on GenerateRequest.Extensions as a provider GenerateOptions
	// extension.
	HostedWebSearch bool `json:"hosted_web_search,omitempty"`
}

// Mode is one thing a model can be asked to do.
//
// The output shapes are the same words the message vocabulary uses for a content
// part; embed and decide are the operations that are not a conversation (a vector,
// a batch of judgments); tool_call is the channel a model answers with calls
// instead of prose. One type for all of them is what lets a grant say "this model,
// for text only" without a second vocabulary.
type Mode string

const (
	// ModeText produces a text answer. It is the default: a model that declares
	// nothing is a text generator, which is what every deployment written before
	// this vocabulary assumed.
	ModeText Mode = Mode(message.PartText)
	// ModeImage, ModeAudio and ModeVideo produce that modality.
	ModeImage Mode = Mode(message.PartImage)
	ModeAudio Mode = Mode(message.PartAudio)
	ModeVideo Mode = Mode(message.PartVideo)
	// ModeToolCall answers with function calls. It rides on text generation rather
	// than replacing it: a request that offers tools needs this channel and the
	// output mode it asked for.
	ModeToolCall Mode = Mode(message.PartToolCall)
	// ModeEmbed turns documents into vectors.
	ModeEmbed Mode = "embed"
	// ModeDecide answers a batch of typed questions.
	ModeDecide Mode = "decide"
)

// Modes lists every mode in declaration order. It exists so a validator and a
// rendering enumerate the set instead of repeating it.
func Modes() []Mode {
	return []Mode{ModeText, ModeImage, ModeAudio, ModeVideo, ModeToolCall, ModeEmbed, ModeDecide}
}

// Valid reports whether m is one of the modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeText, ModeImage, ModeAudio, ModeVideo, ModeToolCall, ModeEmbed, ModeDecide:
		return true
	default:
		return false
	}
}

// Clone returns a defensive copy of the capabilities: the returned value
// shares no backing array with the receiver.
func (c ModelCapabilities) Clone() ModelCapabilities {
	c.Inputs = append([]message.PartKind(nil), c.Inputs...)
	c.Modes = append([]Mode(nil), c.Modes...)
	return c
}

// WithInputs returns a copy of the capabilities with the given input content
// kinds appended. The result shares no backing array with the receiver, so
// calls compose safely.
func (c ModelCapabilities) WithInputs(kinds ...message.PartKind) ModelCapabilities {
	c.Inputs = append(append([]message.PartKind(nil), c.Inputs...), kinds...)
	return c
}

// WithModes returns a copy of the capabilities with the given modes appended. The
// result shares no backing array with the receiver, so calls compose safely.
func (c ModelCapabilities) WithModes(modes ...Mode) ModelCapabilities {
	c.Modes = append(append([]Mode(nil), c.Modes...), modes...)
	return c
}

// EffectiveModes is what the model can actually be asked for.
//
// It is the declared set plus what the other declarations imply — an embedding
// width means the embed mode — and text when nothing at all is declared. That last
// rule is the compatibility bridge: a model that says nothing is a text generator,
// which is what every provider written before this vocabulary meant.
func (c ModelCapabilities) EffectiveModes() []Mode {
	out := make([]Mode, 0, len(c.Modes)+2)
	seen := make(map[Mode]struct{}, len(c.Modes)+2)
	add := func(mode Mode) {
		if _, duplicate := seen[mode]; duplicate {
			return
		}
		seen[mode] = struct{}{}
		out = append(out, mode)
	}
	for _, mode := range c.Modes {
		add(mode)
	}
	if c.EmbeddingDimensions > 0 {
		add(ModeEmbed)
	}
	if len(out) == 0 {
		add(ModeText)
	}
	return out
}

// Declares reports whether the model states anything about what it can serve.
//
// An empty declaration is "unknown", not "text only": a filter must treat it as
// unfiltered, and [ModelCapabilities.EffectiveModes] is the set to render for it. The
// distinction matters for the modes that ride on text — a tool channel nobody mentioned
// is not a tool channel the model lacks.
func (c ModelCapabilities) Declares() bool {
	return len(c.Modes) > 0 || c.EmbeddingDimensions > 0
}

// Can reports whether the model can be asked for mode.
func (c ModelCapabilities) Can(mode Mode) bool {
	for _, declared := range c.EffectiveModes() {
		if declared == mode {
			return true
		}
	}
	return false
}

// WithHostedWebSearch returns a copy of the capabilities with hosted web
// search marked supported.
func (c ModelCapabilities) WithHostedWebSearch() ModelCapabilities {
	c.HostedWebSearch = true
	return c
}

func (c ModelCapabilities) Validate() error {
	if err := validatePartKinds(c.Inputs); err != nil {
		return err
	}
	return validateModes(c.Modes)
}

// validateModes rejects an unknown mode or a repeated one.
func validateModes(modes []Mode) error {
	seen := make(map[Mode]struct{}, len(modes))
	for _, mode := range modes {
		if !mode.Valid() {
			return fmt.Errorf(
				"mode %q is not one of text, image, audio, video, tool_call, embed or decide", mode)
		}
		if _, duplicate := seen[mode]; duplicate {
			return fmt.Errorf("mode %q is declared twice", mode)
		}
		seen[mode] = struct{}{}
	}
	return nil
}

// validatePartKinds checks an input kind list: kinds must be valid and must not repeat.
func validatePartKinds(kinds []message.PartKind) error {
	seen := make(map[message.PartKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if err := kind.Validate(); err != nil {
			return fmt.Errorf("input content kind: %w", err)
		}
		if _, duplicate := seen[kind]; duplicate {
			return fmt.Errorf("duplicate input content kind %q", kind)
		}
		seen[kind] = struct{}{}
	}
	return nil
}
