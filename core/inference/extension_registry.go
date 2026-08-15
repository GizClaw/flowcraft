package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ExtensionDecoder builds one typed extension from its wire-facing
// fields object. Decoding must be strict: unknown fields are typos,
// not optional extras.
type ExtensionDecoder func(fields json.RawMessage) (Extension, error)

// ExtensionDecoderFor adapts a factory for a provider's option struct
// into an ExtensionDecoder. T must be a pointer type whose fields
// carry the provider's JSON contract (e.g. func() *deepseek.GenerateOptions).
func ExtensionDecoderFor[T Extension](factory func() T) ExtensionDecoder {
	return func(fields json.RawMessage) (Extension, error) {
		ext := factory()
		value := reflect.ValueOf(ext)
		if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
			return nil, errdefs.Internalf("extension factory returned %T, want a non-nil pointer", ext)
		}
		dec := json.NewDecoder(bytes.NewReader(fields))
		dec.DisallowUnknownFields()
		if err := dec.Decode(ext); err != nil {
			return nil, errdefs.Validationf("extension fields: %v", err)
		}
		return ext, nil
	}
}

// ExtensionEntry is the wire form of one extension reference: which
// provider's which extension, plus its fields object. Shared by the
// script bridge and graph nodes — anywhere a JSON document needs to
// name a typed extension.
type ExtensionEntry struct {
	Provider string          `json:"provider"`
	ID       string          `json:"id"`
	Fields   json.RawMessage `json:"fields"`
}

// DecodeExtensions resolves entries into typed extensions via the
// registered decoders — by default the provider-carried decoders a
// configured provider registers; a host may add more. Unregistered
// identities are validation errors: the registry is the whole menu.
// A decoder returning an extension whose identity does not match the
// registered key is a wiring bug and surfaces as an internal error.
func DecodeExtensions(entries []ExtensionEntry, decoders map[string]ExtensionDecoder, field string) (Extensions, error) {
	var extensions Extensions
	for i, entry := range entries {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		if entry.Provider == "" || entry.ID == "" {
			return nil, errdefs.Validationf("%s: provider and id are required", entryField)
		}
		key := entry.Provider + "/" + entry.ID
		decoder, ok := decoders[key]
		if !ok {
			return nil, errdefs.Validationf("%s: extension %q is not registered by the host", entryField, key)
		}
		fields := entry.Fields
		if len(fields) == 0 {
			fields = json.RawMessage(`{}`)
		}
		extension, err := decoder(fields)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entryField, err)
		}
		if extension.ProviderID() != entry.Provider || extension.ExtensionID() != entry.ID {
			return nil, errdefs.Internalf(
				"%s: decoder for %q returned extension %q/%q",
				entryField, key, extension.ProviderID(), extension.ExtensionID(),
			)
		}
		extensions = append(extensions, extension)
	}
	return extensions, nil
}
