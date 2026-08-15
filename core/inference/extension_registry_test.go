package inference

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

type registryTestExtension struct {
	Provider string
	Enabled  bool `json:"enabled,omitempty"`
}

func (e registryTestExtension) ProviderID() string             { return e.Provider }
func (e registryTestExtension) ExtensionID() string            { return "generate_options" }
func (e registryTestExtension) ActiveFields() []ExtensionField { return nil }
func (e registryTestExtension) Validate() error                { return nil }
func (e registryTestExtension) Clone() Extension {
	copy := e
	return &copy
}

func registryDecoder(provider string) ExtensionDecoder {
	return ExtensionDecoderFor(func() *registryTestExtension {
		return &registryTestExtension{Provider: provider}
	})
}

func TestExtensionDecoderForStrictDecode(t *testing.T) {
	ext, err := registryDecoder("fake")(json.RawMessage(`{"enabled":true}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	typed, ok := ext.(*registryTestExtension)
	if !ok || !typed.Enabled || typed.Provider != "fake" {
		t.Fatalf("decoded extension = %#v", ext)
	}

	if _, err := registryDecoder("fake")(json.RawMessage(`{"typo":1}`)); err == nil ||
		!errdefs.IsValidation(err) {
		t.Fatalf("unknown field error = %v, want Validation", err)
	}
}

func TestExtensionDecoderForRejectsNonPointerFactory(t *testing.T) {
	decoder := ExtensionDecoderFor(func() registryTestExtension { return registryTestExtension{} })
	if _, err := decoder(json.RawMessage(`{}`)); err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("non-pointer factory error = %v, want Internal", err)
	}
}

func TestDecodeExtensions(t *testing.T) {
	decoders := map[string]ExtensionDecoder{
		"fake/generate_options": registryDecoder("fake"),
	}
	extensions, err := DecodeExtensions([]ExtensionEntry{{
		Provider: "fake",
		ID:       "generate_options",
		Fields:   json.RawMessage(`{"enabled":true}`),
	}}, decoders, "cfg")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	if len(extensions) != 1 {
		t.Fatalf("extensions = %#v, want 1", extensions)
	}
	typed, ok := extensions[0].(*registryTestExtension)
	if !ok || !typed.Enabled || typed.Provider != "fake" {
		t.Fatalf("decoded extension = %#v", extensions[0])
	}
}

func TestDecodeExtensionsDefaultsEmptyFields(t *testing.T) {
	decoders := map[string]ExtensionDecoder{
		"fake/generate_options": registryDecoder("fake"),
	}
	extensions, err := DecodeExtensions([]ExtensionEntry{{
		Provider: "fake",
		ID:       "generate_options",
	}}, decoders, "cfg")
	if err != nil {
		t.Fatalf("DecodeExtensions: %v", err)
	}
	typed := extensions[0].(*registryTestExtension)
	if typed.Enabled {
		t.Fatalf("empty fields decoded as %#v", typed)
	}
}

func TestDecodeExtensionsRequiresProviderAndID(t *testing.T) {
	decoders := map[string]ExtensionDecoder{
		"fake/generate_options": registryDecoder("fake"),
	}
	for _, entry := range []ExtensionEntry{
		{ID: "generate_options"},
		{Provider: "fake"},
	} {
		if _, err := DecodeExtensions([]ExtensionEntry{entry}, decoders, "cfg"); err == nil ||
			!errdefs.IsValidation(err) {
			t.Fatalf("entry %#v error = %v, want Validation", entry, err)
		}
	}
}

func TestDecodeExtensionsUnregisteredIdentity(t *testing.T) {
	decoders := map[string]ExtensionDecoder{
		"fake/generate_options": registryDecoder("fake"),
	}
	_, err := DecodeExtensions([]ExtensionEntry{{
		Provider: "kimi",
		ID:       "generate_options",
	}}, decoders, "cfg")
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "not registered by the host") {
		t.Fatalf("unregistered error = %v, want Validation naming the identity", err)
	}
}

func TestDecodeExtensionsIdentityMismatch(t *testing.T) {
	// The decoder is registered under "other/..." but produces an
	// extension whose ProviderID does not match — a wiring bug.
	decoders := map[string]ExtensionDecoder{
		"other/generate_options": registryDecoder("fake"),
	}
	_, err := DecodeExtensions([]ExtensionEntry{{
		Provider: "other",
		ID:       "generate_options",
	}}, decoders, "cfg")
	if err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("identity mismatch error = %v, want Internal", err)
	}
}
