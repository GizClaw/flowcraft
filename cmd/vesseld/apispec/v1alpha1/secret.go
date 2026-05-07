package v1alpha1

import (
	"encoding/base64"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Secret is the wire form of a credential bundle. The data map
// values are base64-encoded so YAML files round-trip arbitrary
// binary cleanly; stringData is the convenience side-channel for
// plain-text values that the loader merges into data at decode
// time.
//
// Storage semantics: the daemon keeps decoded values in memory
// only. There is no on-disk persistence, no etcd-style cluster
// store, and no encryption-at-rest in v0.1.0 — the user is
// expected to mount the Secret YAMLs from a secure location
// (tmpfs, secrets volume, or generated on boot from a vault).
//
// /v1/plan and `vesseld plan` always replace decoded values with
// the literal "[REDACTED]" before writing them out.
type Secret struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       SecretSpec `json:"spec" yaml:"spec"`
}

type SecretSpec struct {
	// Type categorises the Secret. v0.1.0 only recognises "opaque"
	// (key/value bundles); future types ("tls", "dockerconfigjson")
	// would carry stricter shape validation.
	Type string `json:"type,omitempty" yaml:"type,omitempty"` // opaque (default)

	// Data carries base64-encoded values. The loader merges
	// StringData into Data at decode time after base64-encoding,
	// so consumers only need to look at Data.
	Data map[string]string `json:"data,omitempty" yaml:"data,omitempty"`

	// StringData is the plain-text convenience side-channel.
	// Documented as best-effort: if the same key appears in both
	// Data and StringData, StringData wins (the user clearly
	// intended the readable form).
	StringData map[string]string `json:"stringData,omitempty" yaml:"stringData,omitempty"`
}

func (s Secret) GetTypeMeta() TypeMeta     { return s.TypeMeta }
func (s Secret) GetObjectMeta() ObjectMeta { return s.ObjectMeta }

func (s Secret) Validate() error {
	if err := s.ObjectMeta.Validate(KindSecret); err != nil {
		return err
	}
	if s.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld Secret %q: apiVersion %q != %q", s.Name, s.TypeMeta.APIVersion, APIVersion)
	}
	if s.TypeMeta.Kind != KindSecret {
		return errdefs.Validationf("vesseld Secret %q: kind %q != %q", s.Name, s.TypeMeta.Kind, KindSecret)
	}
	switch s.Spec.Type {
	case "", "opaque":
	default:
		return errdefs.Validationf("vesseld Secret %q: spec.type %q invalid (v0.1.0 only supports opaque)", s.Name, s.Spec.Type)
	}
	for k, v := range s.Spec.Data {
		if k == "" {
			return errdefs.Validationf("vesseld Secret %q: spec.data has empty key", s.Name)
		}
		if _, err := base64.StdEncoding.DecodeString(v); err != nil {
			return errdefs.Validationf("vesseld Secret %q: spec.data[%q] is not valid base64: %v", s.Name, k, err)
		}
	}
	for k := range s.Spec.StringData {
		if k == "" {
			return errdefs.Validationf("vesseld Secret %q: spec.stringData has empty key", s.Name)
		}
	}
	if len(s.Spec.Data) == 0 && len(s.Spec.StringData) == 0 {
		return errdefs.Validationf("vesseld Secret %q: spec.data or spec.stringData must contain at least one entry", s.Name)
	}
	return nil
}

// MergedData returns a fresh map of base64-decoded plain-text
// values combining Data and StringData (StringData wins). Returns
// errdefs.Validation if any base64 entry is malformed (validate is
// expected to have caught this; the helper double-checks for
// safety against API consumers that bypass Validate).
func (s Secret) MergedData() (map[string]string, error) {
	out := make(map[string]string, len(s.Spec.Data)+len(s.Spec.StringData))
	for k, b64 := range s.Spec.Data {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, errdefs.Validationf("vesseld Secret %q: data[%q] base64 decode: %v", s.Name, k, err)
		}
		out[k] = string(raw)
	}
	for k, v := range s.Spec.StringData {
		out[k] = v
	}
	return out, nil
}
