package inference_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
)

// stubExtension is the smallest extension a registry entry can resolve to.
type stubExtension struct {
	provider string
	id       string
	value    string
	fields   []inference.ExtensionField
}

func (s stubExtension) ProviderID() string  { return s.provider }
func (s stubExtension) ExtensionID() string { return s.id }
func (s stubExtension) ActiveFields() []inference.ExtensionField {
	if s.fields != nil {
		return s.fields
	}
	return []inference.ExtensionField{"value"}
}
func (s stubExtension) Validate() error            { return nil }
func (s stubExtension) Clone() inference.Extension { return s }

// TestDecodeStrictRejectsUnknownFields is the property that makes a configuration
// typo an error instead of a silently dropped option.
func TestDecodeStrictRejectsUnknownFields(t *testing.T) {
	type fields struct {
		Value string `json:"value"`
	}

	var good fields
	if err := inference.DecodeStrict([]byte(`{"value":"x"}`), &good); err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if good.Value != "x" {
		t.Fatalf("Value = %q", good.Value)
	}

	var typed fields
	if err := inference.DecodeStrict([]byte(`{"value":"x","valu":"typo"}`), &typed); err == nil {
		t.Fatal("DecodeStrict accepted an unknown field")
	}
	var malformed fields
	if err := inference.DecodeStrict([]byte(`{"value":`), &malformed); err == nil {
		t.Fatal("DecodeStrict accepted malformed JSON")
	}
}

// TestDecodeExtensions resolves a deployment's entries into typed values, and
// refuses everything the registry cannot honour: an unregistered identity would
// otherwise look configured and behave as if it were not.
func TestDecodeExtensions(t *testing.T) {
	decoders := map[string]inference.ExtensionDecoder{
		"openai/verbosity": func(fields json.RawMessage) (inference.Extension, error) {
			return stubExtension{provider: "openai", id: "verbosity", value: string(fields)}, nil
		},
		"openai/failing": func(json.RawMessage) (inference.Extension, error) {
			return nil, errors.New("bad fields")
		},
		"openai/nil": func(json.RawMessage) (inference.Extension, error) { return nil, nil },
		"openai/impostor": func(json.RawMessage) (inference.Extension, error) {
			return stubExtension{provider: "other", id: "thing"}, nil
		},
	}

	t.Run("empty stays nil", func(t *testing.T) {
		got, err := inference.DecodeExtensions(nil, decoders, "extensions")
		if err != nil || got != nil {
			t.Fatalf("got %v, %v; want nil, nil", got, err)
		}
	})

	t.Run("resolves a registered entry", func(t *testing.T) {
		got, err := inference.DecodeExtensions([]inference.ExtensionEntry{
			{Provider: "openai", ID: "verbosity", Fields: []byte(`{"value":"low"}`)},
		}, decoders, "extensions")
		if err != nil {
			t.Fatalf("DecodeExtensions: %v", err)
		}
		if len(got) != 1 || got[0].ExtensionID() != "verbosity" {
			t.Fatalf("got %v", got)
		}
	})

	for _, tc := range []struct {
		name    string
		entries []inference.ExtensionEntry
	}{
		{"missing provider", []inference.ExtensionEntry{{ID: "verbosity"}}},
		{"missing id", []inference.ExtensionEntry{{Provider: "openai"}}},
		{"duplicate entry", []inference.ExtensionEntry{
			{Provider: "openai", ID: "verbosity"},
			{Provider: "openai", ID: "verbosity"},
		}},
		{"unregistered identity", []inference.ExtensionEntry{{Provider: "unknown", ID: "thing"}}},
		{"decoder failed", []inference.ExtensionEntry{{Provider: "openai", ID: "failing"}}},
		{"decoder returned nil", []inference.ExtensionEntry{{Provider: "openai", ID: "nil"}}},
		{"decoder lied about its identity", []inference.ExtensionEntry{{Provider: "openai", ID: "impostor"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := inference.DecodeExtensions(tc.entries, decoders, "extensions"); err == nil {
				t.Fatal("DecodeExtensions accepted an entry the registry could not honour")
			}
		})
	}

	// The two refusals differ in kind: a deployment naming an unknown option is a
	// validation problem, while a decoder that produced the wrong value is a bug.
	t.Run("kinds differ", func(t *testing.T) {
		_, err := inference.DecodeExtensions([]inference.ExtensionEntry{{Provider: "unknown", ID: "thing"}}, decoders, "extensions")
		if !errdefs.IsValidation(err) {
			t.Fatalf("unregistered identity = %v, want a validation error", err)
		}
		_, err = inference.DecodeExtensions([]inference.ExtensionEntry{{Provider: "openai", ID: "impostor"}}, decoders, "extensions")
		if !errdefs.IsInternal(err) {
			t.Fatalf("impostor decoder = %v, want an internal error", err)
		}
	})
}

// TestDecodeExtensionsRejectsAnEmptyActiveFieldSet keeps the registry honest: an
// extension that claims no option is indistinguishable from one that set nothing,
// so the contract check would never see it.
func TestDecodeExtensionsRejectsAnEmptyActiveFieldSet(t *testing.T) {
	decoders := map[string]inference.ExtensionDecoder{
		"openai/empty": func(json.RawMessage) (inference.Extension, error) {
			return stubExtension{provider: "openai", id: "empty", fields: []inference.ExtensionField{}}, nil
		},
	}
	_, err := inference.DecodeExtensions(
		[]inference.ExtensionEntry{{Provider: "openai", ID: "empty"}}, decoders, "extensions")
	if err == nil {
		t.Fatal("an extension with no active fields was accepted")
	}
	if !strings.Contains(err.Error(), "no active fields") {
		t.Fatalf("err = %v, want it to name the empty field set", err)
	}
}
