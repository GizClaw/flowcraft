package model

import (
	"reflect"
	"testing"
)

// TestModesEnumerateThemselves keeps the vocabulary and its validator in one place: a
// mode added to the constants but not to the list would never be validated, and a
// dashboard would report a value the package cannot produce.
func TestModesEnumerateThemselves(t *testing.T) {
	modes := Modes()
	if len(modes) != 7 {
		t.Fatalf("Modes() = %v, want the seven a grant can name", modes)
	}
	seen := make(map[Mode]bool, len(modes))
	for _, mode := range modes {
		if !mode.Valid() || seen[mode] {
			t.Fatalf("mode %q is invalid or duplicated", mode)
		}
		seen[mode] = true
	}
	for _, mode := range []Mode{ModeText, ModeImage, ModeAudio, ModeVideo, ModeToolCall, ModeEmbed, ModeDecide} {
		if !mode.Valid() {
			t.Fatalf("listed mode %q reports itself invalid", mode)
		}
	}
	if Mode("file").Valid() || Mode("").Valid() {
		t.Fatal("an unlisted mode reported itself valid")
	}
}

// TestEffectiveModesFoldsTheDeclarations is the compatibility bridge: the modes are what
// the model says, what the other declarations imply, and text when it says nothing at all.
func TestEffectiveModesFoldsTheDeclarations(t *testing.T) {
	for name, tc := range map[string]struct {
		capabilities ModelCapabilities
		want         []Mode
	}{
		"nothing declared is a text generator": {ModelCapabilities{}, []Mode{ModeText}},
		"declared modes are kept":              {ModelCapabilities{Modes: []Mode{ModeImage}}, []Mode{ModeImage}},
		"an embedding width implies embed": {
			ModelCapabilities{EmbeddingDimensions: 1536},
			[]Mode{ModeEmbed},
		},
		"tools ride on text": {
			ModelCapabilities{Modes: []Mode{ModeText, ModeToolCall}},
			[]Mode{ModeText, ModeToolCall},
		},
		"declarations combine without repeating": {
			ModelCapabilities{Modes: []Mode{ModeText}, EmbeddingDimensions: 8},
			[]Mode{ModeText, ModeEmbed},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.capabilities.EffectiveModes(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EffectiveModes = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCanReadsTheEffectiveSet pins the predicate the router uses: an undeclared model can
// generate text and nothing else.
func TestCanReadsTheEffectiveSet(t *testing.T) {
	var quiet ModelCapabilities
	if !quiet.Can(ModeText) {
		t.Fatal("an undeclared model cannot generate text")
	}
	for _, mode := range []Mode{ModeImage, ModeAudio, ModeVideo, ModeToolCall, ModeEmbed, ModeDecide} {
		if quiet.Can(mode) {
			t.Fatalf("an undeclared model claimed %q", mode)
		}
	}
	embedding := ModelCapabilities{EmbeddingDimensions: 4}
	if !embedding.Can(ModeEmbed) || embedding.Can(ModeText) {
		t.Fatalf("embedding capabilities = %v", embedding.EffectiveModes())
	}
}

// TestDeclaresSeparatesUnknownFromTextOnly is the declaration rule made explicit: an
// empty capability block is unknown, while one that names a mode is a statement — and a
// tool channel nobody mentioned is not a tool channel the model lacks.
func TestDeclaresSeparatesUnknownFromTextOnly(t *testing.T) {
	if (ModelCapabilities{}).Declares() {
		t.Fatal("an empty declaration reported itself as declared")
	}
	for name, capabilities := range map[string]ModelCapabilities{
		"a declared mode":    {Modes: []Mode{ModeText}},
		"an embedding width": {EmbeddingDimensions: 8},
	} {
		if !capabilities.Declares() {
			t.Fatalf("%s did not report itself declared", name)
		}
	}
}
