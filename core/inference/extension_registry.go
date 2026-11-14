package inference

import (
	"bytes"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/goccy/go-json"
)

// ExtensionDecoder builds one typed extension from its wire-facing fields
// object. Decoding must be strict: an unknown field is a typo, not an optional
// extra, and a typo that decodes silently is exactly the dropped option the
// contract check cannot see.
type ExtensionDecoder func(fields json.RawMessage) (Extension, error)

// ExtensionEntry is the config-side reference to one extension: which provider's
// which extension, plus the fields object a deployment wrote.
type ExtensionEntry struct {
	Provider string          `json:"provider" yaml:"provider"`
	ID       string          `json:"id" yaml:"id"`
	Fields   json.RawMessage `json:"fields,omitempty" yaml:"fields,omitempty"`
}

// Key is the registry key an entry resolves against.
func (e ExtensionEntry) Key() string { return e.Provider + "/" + e.ID }

// DecodeExtensions resolves entries into typed extensions through the registered
// decoders. The registry is the whole menu: an unregistered identity is a
// validation error rather than an ignored value, because a deployment that names
// an option the running build does not know would otherwise look configured and
// behave as if it were not.
//
// field names the config location in errors ("inference node extensions").
func DecodeExtensions(
	entries []ExtensionEntry,
	decoders map[string]ExtensionDecoder,
	field string,
) (Extensions, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	extensions := make(Extensions, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		location := fmt.Sprintf("%s[%d]", field, i)
		if entry.Provider == "" || entry.ID == "" {
			return nil, errdefs.Validationf("%s: provider and id are required", location)
		}
		key := entry.Key()
		if _, ok := seen[key]; ok {
			return nil, errdefs.Validationf("%s: duplicate extension %q", location, key)
		}
		seen[key] = struct{}{}

		decoder, ok := decoders[key]
		if !ok {
			return nil, errdefs.Validationf(
				"%s: extension %q is not registered", location, key,
			)
		}
		extension, err := decoder(entry.Fields)
		if err != nil {
			return nil, errdefs.Validationf("%s: %w", location, err)
		}
		if extension == nil {
			return nil, errdefs.Internalf("%s: decoder for %q returned nil", location, key)
		}
		if got := extension.ProviderID() + "/" + extension.ExtensionID(); got != key {
			return nil, errdefs.Internalf(
				"%s: decoder for %q produced identity %q", location, key, got,
			)
		}
		extensions = append(extensions, extension)
	}
	if err := extensions.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: %w", field, err)
	}
	return extensions, nil
}

// DecodeStrict decodes fields into target, rejecting unknown keys. It exists so a
// provider's decoder does not have to remember to be strict.
func DecodeStrict(fields json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(fields))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("extension fields: %w", err)
	}
	return nil
}
