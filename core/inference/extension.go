package inference

import (
	"fmt"
	"regexp"

	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// Extension carries provider-specific request options. It exists so a caller
// can reach a knob the canonical vocabulary does not model (a vendor's
// reasoning budget, a vendor's safety level) without widening
// [GenerateRequest] with every provider's parameters, and without an untyped
// map that would let a misspelled key become a silent no-op.
//
// Implementations live in driver packages. The identity pair
// (ProviderID, ExtensionID) is the address a driver uses to prove it consumed
// the value: a driver strips its own extensions during compilation and reports
// the rest as inert.
type Extension interface {
	// ProviderID names the provider this extension belongs to.
	ProviderID() string
	// ExtensionID names the extension within that provider.
	ExtensionID() string
	// ActiveFields lists the options this value actually sets. Comparing it
	// against what the compiler consumed is how a dropped option is caught.
	ActiveFields() []ExtensionField
	// Validate checks the value's own invariants.
	Validate() error
	// Clone returns a deep copy that shares no mutable state.
	Clone() Extension
}

// ExtensionField is an option name local to one extension. It is qualified with
// the extension identity before it enters the canonical [Field] space.
type ExtensionField string

// Qualify turns a local option name into the canonical field identity
// "extension.<provider>.<extension>.<option>".
func (field ExtensionField) Qualify(extension Extension) Field {
	return Field(
		extensionFieldPrefix +
			extension.ProviderID() + "." +
			extension.ExtensionID() + "." +
			string(field),
	)
}

// Extensions is the ordered set of provider-specific options on one request.
type Extensions []Extension

// Clone returns a deep copy, preserving order and nil-ness.
func (extensions Extensions) Clone() Extensions {
	if extensions == nil {
		return nil
	}
	cloned := make(Extensions, len(extensions))
	for i, extension := range extensions {
		if !ptr.IsNil(extension) {
			cloned[i] = extension.Clone()
		}
	}
	return cloned
}

var extensionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// extensionFieldPattern allows dots inside an option name: a provider may model a
// nested option path (an escape hatch's "json.<key>" is one), and the dotted form
// is also what the qualified canonical identity uses.
var extensionFieldPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Validate rejects nil extensions, malformed identities, duplicates and
// extensions that claim no active field.
func (extensions Extensions) Validate() error {
	seen := make(map[string]struct{}, len(extensions))
	for i, extension := range extensions {
		if ptr.IsNil(extension) {
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
			if !extensionFieldPattern.MatchString(string(field)) {
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

// ForProvider returns the extensions addressed to provider, preserving order.
// Extensions addressed elsewhere are inert: a caller may carry options for
// several providers at once, and each attempt applies only its own.
func (extensions Extensions) ForProvider(provider string) Extensions {
	var filtered Extensions
	for _, extension := range extensions {
		if ptr.IsNil(extension) || extension.ProviderID() != provider {
			continue
		}
		filtered = append(filtered, extension)
	}
	return filtered
}

// ActiveFields returns the qualified identity of every option the set claims.
func (extensions Extensions) ActiveFields() []Field {
	var fields []Field
	for _, extension := range extensions {
		if ptr.IsNil(extension) {
			continue
		}
		for _, field := range extension.ActiveFields() {
			fields = append(fields, field.Qualify(extension))
		}
	}
	return fields
}
