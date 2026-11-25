package route

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

// catalog is a provider's published line-up, for a target that declares inputs.
type testCatalog struct{ models []model.ModelDescriptor }

func (c testCatalog) Models() []model.ModelDescriptor { return c.models }
func (c testCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, d := range c.models {
		if d.ID == id {
			return d, true
		}
	}
	return model.ModelDescriptor{}, false
}

func targetDeclaring(name string, inputs ...message.PartKind) Target {
	return Target{
		Name:  "t-" + name,
		Model: model.ModelID{Provider: "p", Name: name},
		Provider: inference.Provider{Catalog: testCatalog{models: []model.ModelDescriptor{{
			ID:           model.ModelID{Provider: "p", Name: name},
			Capabilities: model.ModelCapabilities{Inputs: inputs},
		}}}},
	}
}

func contentOf(kinds ...message.PartKind) message.Content {
	var content message.Content
	for _, kind := range kinds {
		switch kind {
		case message.PartImage:
			content.Parts = append(content.Parts, message.ImagePart{})
		default:
			content.Parts = append(content.Parts, message.TextPart{Text: "x"})
		}
	}
	return content
}

// TestDeclaredInputsAreAHardFilter is the promise the capabilities type makes: a model that
// declares one input kind is not handed another and left to fail on the wire, where the error
// reads like a provider fault.
func TestDeclaredInputsAreAHardFilter(t *testing.T) {
	textOnly := targetDeclaring("text-only", message.PartText)
	multimodal := targetDeclaring("multimodal", message.PartText, message.PartImage)
	undeclared := targetDeclaring("undeclared")
	image := contentOf(message.PartText, message.PartImage)

	for _, tc := range []struct {
		name    string
		targets []Target
		content message.Content
		want    []string
		wantErr bool
	}{
		{"declared text refuses an image", []Target{textOnly, multimodal}, image, []string{"t-multimodal"}, false},
		{"empty declaration filters nothing", []Target{undeclared}, image, []string{"t-undeclared"}, false},
		{"text survives a text-only target", []Target{textOnly}, contentOf(message.PartText), []string{"t-text-only"}, false},
		{"no target carries it", []Target{textOnly}, image, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, err := acceptContent(tc.targets, model.ModelID{}, tc.content)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("acceptContent accepted %v", tc.targets)
				}
				var classified *inference.Error
				if !errors.As(err, &classified) || classified.Kind != inference.UnsupportedFeature {
					t.Fatalf("err = %v, want UnsupportedFeature", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("acceptContent: %v", err)
			}
			if len(kept) != len(tc.want) {
				t.Fatalf("kept %d targets, want %d", len(kept), len(tc.want))
			}
			for i, target := range kept {
				if target.Name != tc.want[i] {
					t.Fatalf("kept[%d] = %q, want %q (order is declaration order)", i, target.Name, tc.want[i])
				}
			}
		})
	}
}

// TestAnOutputModeIsAHardFilter is the other half of the capabilities contract: a
// model that cannot produce the requested modality is not a candidate. The historical
// default still holds — a model that declares nothing is a text generator — so a fleet
// with no declarations keeps working for text and refuses what it never claimed.
func TestAnOutputModeIsAHardFilter(t *testing.T) {
	// One model, three targets that declare different things about it: the filter is
	// about the model, not about which target was asked.
	shared := model.ModelID{Provider: "p", Name: "m"}
	generating := &fakeGenerator{}
	withModes := func(name string, modes ...model.Mode) Target {
		return Target{Name: name, Provider: inference.Provider{
			Generator: generating,
			Catalog: testCatalog{models: []model.ModelDescriptor{{
				ID:           shared,
				Capabilities: model.ModelCapabilities{Modes: modes},
			}}},
		}}
	}
	textOut := withModes("text-out", model.ModeText)
	imageOut := withModes("image-out", model.ModeText, model.ModeImage)
	// A generator that declares no line-up: the provider's own modes are the answer,
	// and it is a candidate because it is named for the requested provider.
	undeclared := Target{Name: shared.Provider, Provider: inference.Provider{Generator: generating}}

	for _, tc := range []struct {
		name    string
		targets []Target
		mode    model.Mode
		want    []string
	}{
		{"an image request keeps only the image model", []Target{textOut, imageOut}, model.ModeImage, []string{"image-out"}},
		{"a text request keeps both", []Target{textOut, imageOut}, model.ModeText, []string{"text-out", "image-out"}},
		{"an undeclared model on a generator is text", []Target{undeclared}, model.ModeText, []string{shared.Provider}},
		{"and nothing else", []Target{undeclared}, model.ModeImage, nil},
		{"the embed mode needs a declaration", []Target{textOut}, model.ModeEmbed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := filterTargets(tc.targets, model.ModelRef{ID: shared}, []model.Mode{tc.mode})
			if len(kept) != len(tc.want) {
				t.Fatalf("kept %d targets, want %d", len(kept), len(tc.want))
			}
			for i, target := range kept {
				if target.Name != tc.want[i] {
					t.Fatalf("kept[%d] = %q, want %q", i, target.Name, tc.want[i])
				}
			}
		})
	}
}
