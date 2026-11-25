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
			kept, err := acceptContent(tc.targets, tc.content)
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

// TestOutputsAreAPreferenceNotAPermission is the half of the capabilities comment that must never
// turn into a rejection: a target that declares only text still runs an image request when nothing
// better exists, and a fleet that declares no outputs keeps its declaration order.
func TestOutputsAreAPreferenceNotAPermission(t *testing.T) {
	textOut := targetDeclaring("text-out")
	textOut.Provider.Catalog = testCatalog{models: []model.ModelDescriptor{{
		ID:           textOut.Model,
		Capabilities: model.ModelCapabilities{Outputs: []message.PartKind{message.PartText}},
	}}}
	imageOut := targetDeclaring("image-out")
	imageOut.Provider.Catalog = testCatalog{models: []model.ModelDescriptor{{
		ID:           imageOut.Model,
		Capabilities: model.ModelCapabilities{Outputs: []message.PartKind{message.PartText, message.PartImage}},
	}}}
	undeclared := targetDeclaring("undeclared")

	for _, tc := range []struct {
		name    string
		targets []Target
		want    inference.Output
		first   string
	}{
		{"image intent prefers the image model", []Target{textOut, imageOut}, inference.OutputImage, "t-image-out"},
		{"text intent keeps declaration order", []Target{textOut, imageOut}, inference.OutputText, "t-text-out"},
		{"undeclared covers everything", []Target{undeclared, textOut}, inference.OutputImage, "t-undeclared"},
		{"nothing is dropped", []Target{textOut, imageOut}, inference.OutputImage, "t-image-out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ordered := preferOutputs(tc.targets, tc.want)
			if len(ordered) != len(tc.targets) {
				t.Fatalf("preferOutputs dropped %d of %d targets", len(tc.targets)-len(ordered), len(tc.targets))
			}
			if ordered[0].Name != tc.first {
				t.Fatalf("first = %q, want %q", ordered[0].Name, tc.first)
			}
		})
	}
}
