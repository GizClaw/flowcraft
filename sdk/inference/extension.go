package inference

import (
	"fmt"
	"regexp"
)

// Extension is implemented by typed values in provider packages. The
// provider-qualified key lets the compiler prove that each extension was
// consumed without exposing an untyped parameter map.
type Extension interface {
	ProviderID() string
	ExtensionID() string
	ActiveFields() []ExtensionField
	Validate() error
	Clone() Extension
}

// ExtensionField is an active provider-specific option name local to one
// extension. Runtime qualifies it with the provider and extension identities
// before requiring a compiler disposition.
type ExtensionField string

func (field ExtensionField) Qualify(extension Extension) FieldID {
	return FieldID(
		"extension." +
			extension.ProviderID() + "." +
			extension.ExtensionID() + "." +
			string(field),
	)
}

type Extensions []Extension

func (extensions Extensions) Clone() Extensions {
	cloned := make(Extensions, len(extensions))
	for i, extension := range extensions {
		if !isNilValue(extension) {
			cloned[i] = extension.Clone()
		}
	}
	return cloned
}

var extensionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func (extensions Extensions) Validate() error {
	seen := make(map[string]struct{}, len(extensions))
	for i, extension := range extensions {
		if isNilValue(extension) {
			return fmt.Errorf("extension %d is nil", i)
		}
		if !extensionIDPattern.MatchString(extension.ProviderID()) ||
			!extensionIDPattern.MatchString(extension.ExtensionID()) {
			return fmt.Errorf("extension %d has invalid identity", i)
		}
		key := extension.ProviderID() + "/" + extension.ExtensionID()
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate extension %q", key)
		}
		seen[key] = struct{}{}
		if err := extension.Validate(); err != nil {
			return fmt.Errorf("extension %q: %w", key, err)
		}
		fields := extension.ActiveFields()
		if len(fields) == 0 {
			return fmt.Errorf("extension %q has no active fields", key)
		}
		seenFields := make(map[ExtensionField]struct{}, len(fields))
		for _, field := range fields {
			if !extensionIDPattern.MatchString(string(field)) {
				return fmt.Errorf("extension %q has invalid active field %q", key, field)
			}
			if _, ok := seenFields[field]; ok {
				return fmt.Errorf("extension %q has duplicate active field %q", key, field)
			}
			seenFields[field] = struct{}{}
		}
	}
	return nil
}

func (extensions Extensions) ValidateForProvider(provider string) error {
	if err := extensions.Validate(); err != nil {
		return err
	}
	for _, extension := range extensions {
		if extension.ProviderID() != provider {
			return fmt.Errorf(
				"extension %q belongs to provider %q, not %q",
				extension.ExtensionID(),
				extension.ProviderID(),
				provider,
			)
		}
	}
	return nil
}

func (extensions Extensions) AppendActiveFields(fields []FieldID) []FieldID {
	return append(fields, extensions.ActiveFields()...)
}

func (extensions Extensions) ActiveFields() []FieldID {
	var fields []FieldID
	for _, extension := range extensions {
		if isNilValue(extension) {
			continue
		}
		for _, field := range extension.ActiveFields() {
			fields = append(fields, field.Qualify(extension))
		}
	}
	return fields
}
