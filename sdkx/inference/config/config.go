// Package config loads versioned, secret-reference-only deployment
// configuration and materializes immutable inference provider definitions.
//
// Provider-specific packages own the interpretation of ProviderConfig.Spec
// and ProfileConfig.Spec. This package deliberately does not define model
// capability flags or provider wire rules.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

const VersionV1 = "v1"

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Document is one complete, immutable deployment snapshot.
type Document struct {
	Version   string           `json:"version"`
	Providers []ProviderConfig `json:"providers"`
}

// ProviderConfig is the provider-neutral envelope consumed by a provider
// Factory. Spec contains provider-owned, non-secret configuration.
type ProviderConfig struct {
	ID       string          `json:"id"`
	Driver   string          `json:"driver"`
	Profiles []ProfileConfig `json:"profiles,omitempty"`
	Spec     json.RawMessage `json:"spec,omitempty"`
}

// ProfileConfig declares one credential profile. Secrets contains references,
// never resolved values. Spec contains provider-owned, non-secret settings
// such as endpoints, organizations, or projects.
type ProfileConfig struct {
	ID         string                `json:"id,omitempty"`
	Operations []inference.Operation `json:"operations,omitempty"`
	Secrets    map[string]SecretRef  `json:"secrets,omitempty"`
	Spec       json.RawMessage       `json:"spec,omitempty"`
}

// SecretRef identifies a value in an explicitly supplied SecretResolver.
type SecretRef struct {
	Resolver string `json:"resolver" yaml:"resolver"`
	Key      string `json:"key" yaml:"key"`
}

func DecodeJSON(reader io.Reader) (Document, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode inference configuration: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return Document{}, fmt.Errorf(
			"decode inference configuration: %w",
			err,
		)
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

// DecodeSpec lets a provider factory strictly decode its own typed Spec.
// Provider configuration types should deliberately omit credentials: resolved
// secrets arrive separately through ResolvedProfile.Secrets.
func DecodeSpec[T any](raw json.RawMessage) (T, error) {
	var target T
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&target); err != nil {
		return target, fmt.Errorf("decode provider spec: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return target, fmt.Errorf("decode provider spec: %w", err)
	}
	return target, nil
}

func (d Document) Validate() error {
	if d.Version != VersionV1 {
		return fmt.Errorf(
			"inference configuration has unsupported version %q",
			d.Version,
		)
	}
	if len(d.Providers) == 0 {
		return fmt.Errorf("inference configuration requires a provider")
	}
	providers := make(map[string]struct{}, len(d.Providers))
	for index, provider := range d.Providers {
		if err := provider.validate(); err != nil {
			return fmt.Errorf("provider %d: %w", index, err)
		}
		if _, ok := providers[provider.ID]; ok {
			return fmt.Errorf("duplicate provider %q", provider.ID)
		}
		providers[provider.ID] = struct{}{}
	}
	return nil
}

func (p ProviderConfig) validate() error {
	if !identifierPattern.MatchString(p.ID) {
		return fmt.Errorf("invalid provider ID %q", p.ID)
	}
	if !identifierPattern.MatchString(p.Driver) {
		return fmt.Errorf(
			"provider %q has invalid driver %q",
			p.ID,
			p.Driver,
		)
	}
	if err := validateSpec("provider spec", p.Spec); err != nil {
		return fmt.Errorf("provider %q: %w", p.ID, err)
	}
	profiles := make(map[string]struct{}, len(p.Profiles))
	for index, profile := range p.Profiles {
		if err := profile.validate(); err != nil {
			return fmt.Errorf(
				"provider %q profile %d: %w",
				p.ID,
				index,
				err,
			)
		}
		if _, ok := profiles[profile.ID]; ok {
			return fmt.Errorf(
				"provider %q has duplicate profile %q",
				p.ID,
				profile.ID,
			)
		}
		profiles[profile.ID] = struct{}{}
	}
	return nil
}

func (p ProfileConfig) validate() error {
	if p.ID != "" && !identifierPattern.MatchString(p.ID) {
		return fmt.Errorf("invalid profile ID %q", p.ID)
	}
	operations := make(map[inference.Operation]struct{}, len(p.Operations))
	for _, operation := range p.Operations {
		if err := operation.Validate(); err != nil {
			return err
		}
		if _, ok := operations[operation]; ok {
			return fmt.Errorf("duplicate profile operation %q", operation)
		}
		operations[operation] = struct{}{}
	}
	for name, ref := range p.Secrets {
		if !identifierPattern.MatchString(name) {
			return fmt.Errorf("invalid secret name %q", name)
		}
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("secret %q: %w", name, err)
		}
	}
	return validateSpec("profile spec", p.Spec)
}

func (r SecretRef) Validate() error {
	if !identifierPattern.MatchString(r.Resolver) {
		return fmt.Errorf("invalid secret resolver %q", r.Resolver)
	}
	if r.Key == "" {
		return fmt.Errorf("secret key is required")
	}
	return nil
}

func validateSpec(label string, spec json.RawMessage) error {
	if len(spec) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(spec))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return fmt.Errorf("%s must be a JSON object: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("%s must be a JSON object: %w", label, err)
	}
	if object == nil {
		return fmt.Errorf("%s must be a JSON object", label)
	}
	return validateCredentialFreeSpec(label, object)
}

var credentialFieldNames = map[string]struct{}{
	"apikey":        {},
	"accesskey":     {},
	"accesstoken":   {},
	"authorization": {},
	"clientsecret":  {},
	"credential":    {},
	"credentials":   {},
	"password":      {},
	"privatekey":    {},
	"refreshtoken":  {},
	"secret":        {},
	"secrets":       {},
	"token":         {},
}

var credentialKeyNormalizer = strings.NewReplacer(
	"-", "",
	"_", "",
	".", "",
	" ", "",
)

func validateCredentialFreeSpec(label string, value any) error {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			normalized := strings.ToLower(
				credentialKeyNormalizer.Replace(key),
			)
			if _, forbidden := credentialFieldNames[normalized]; forbidden {
				return fmt.Errorf(
					"%s contains credential field %q; use SecretRef",
					label,
					key,
				)
			}
			if err := validateCredentialFreeSpec(label, child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range current {
			if err := validateCredentialFreeSpec(label, child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d Document) Clone() Document {
	cloned := Document{
		Version:   d.Version,
		Providers: make([]ProviderConfig, len(d.Providers)),
	}
	for index, provider := range d.Providers {
		cloned.Providers[index] = provider.Clone()
	}
	return cloned
}

func (p ProviderConfig) Clone() ProviderConfig {
	p.Spec = append(json.RawMessage(nil), p.Spec...)
	profiles := make([]ProfileConfig, len(p.Profiles))
	for index, profile := range p.Profiles {
		profiles[index] = profile.Clone()
	}
	p.Profiles = profiles
	return p
}

func (p ProfileConfig) Clone() ProfileConfig {
	p.Operations = append([]inference.Operation(nil), p.Operations...)
	p.Spec = append(json.RawMessage(nil), p.Spec...)
	secrets := make(map[string]SecretRef, len(p.Secrets))
	for name, ref := range p.Secrets {
		secrets[name] = ref
	}
	p.Secrets = secrets
	return p
}
