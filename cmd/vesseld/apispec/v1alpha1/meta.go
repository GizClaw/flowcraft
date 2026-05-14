package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// APIVersion is the literal apiVersion string every v1alpha1
// document carries. Defined as a constant so callers can compare
// without typo risk and so the decoder router has one canonical
// value to dispatch on.
const APIVersion = "vessel.flowcraft.io/v1alpha1"

// Known kinds exposed by v1alpha1. Listed as a constant block so
// the validate / decode paths can range over them and so a typo in
// any kind name shows up as a missing constant rather than a
// silent string mismatch.
const (
	KindDaemon       = "Daemon"
	KindVessel       = "Vessel"
	KindAgent        = "Agent"
	KindLLMProfile   = "LLMProfile"
	KindProbe        = "Probe"
	KindToolPack     = "ToolPack"
	KindHistoryStore = "HistoryStore"
	KindSecret       = "Secret"
	KindSandbox      = "Sandbox"
)

// AllKinds returns every well-known kind registered for this
// apiVersion. Used by the loader for "is this a kind we recognise"
// checks and by docs / CLI help to list supported kinds.
func AllKinds() []string {
	return []string{
		KindDaemon, KindVessel, KindAgent, KindLLMProfile,
		KindProbe, KindToolPack, KindHistoryStore, KindSecret,
		KindSandbox,
	}
}

// TypeMeta is the apiVersion + kind discriminator embedded by every
// kind struct. Mirrors the upstream convention (apiVersion+kind on
// every document) so the YAML wire form looks familiar to anyone
// who has authored declarative config before.
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

// ObjectMeta is the metadata block shared by every kind. Fields are
// deliberately limited in v0.1.0:
//
//   - Name is the per-kind unique identifier; the loader fails if
//     two documents with the same apiVersion+kind+name appear.
//   - Labels and Annotations are reserved: the daemon does not
//     interpret them in v0.1.0. Decoding accepts them so users
//     can start adopting them now without breaking on upgrade.
//
// The reason we accept (but ignore) labels/annotations is to keep
// configuration files forward-compatible: when v0.2.0 introduces
// label-based vessel filtering for the API surface, no users have
// to rewrite their YAML.
type ObjectMeta struct {
	Name        string            `json:"name" yaml:"name"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// Validate enforces the minimal name rules the loader relies on:
// non-empty, no '/' (the runtime uses "vesselID/agentName" as a
// composite key in several places). v1alpha1 keeps the rules
// permissive on purpose — RFC 1123 label restrictions can land in
// v1beta1 once the user base has settled on the looser form.
func (m ObjectMeta) Validate(kind string) error {
	if m.Name == "" {
		return errdefs.Validationf("vesseld %s: metadata.name is empty", kind)
	}
	for _, r := range m.Name {
		if r == '/' || r == ' ' {
			return errdefs.Validationf("vesseld %s: metadata.name %q contains forbidden character %q", kind, m.Name, r)
		}
	}
	return nil
}

// ValueSource is the discriminated union for any field that may
// pull its value from a non-inline source. Used by anything secret
// or environment-dependent (LLM API keys, database passwords).
//
// Exactly one of the three sub-fields must be set; setting more
// than one is a validation error so callers cannot accidentally
// silently fall back from secretRef to env.
//
// Inline plain-text values are NOT a fourth option: any place we
// accept ValueSource we explicitly reject inline strings, so a
// freshly-cloned config repository never accidentally leaks
// credentials through git history.
type ValueSource struct {
	Env       string           `json:"env,omitempty" yaml:"env,omitempty"`
	File      string           `json:"file,omitempty" yaml:"file,omitempty"`
	SecretRef *SecretReference `json:"secretRef,omitempty" yaml:"secretRef,omitempty"`
}

// SecretReference points at a v1alpha1 Secret resource by name +
// data-map key. Resolved by the resolver against the loaded Secret
// inventory; the daemon never reads the cluster filesystem at
// resolution time except to satisfy a top-level File source.
type SecretReference struct {
	Name string `json:"name" yaml:"name"`
	Key  string `json:"key" yaml:"key"`
}

// ValueRef wraps a ValueSource so the YAML form is uniform across
// every secret-bearing field:
//
//	apiKey:
//	  valueFrom:
//	    env: OPENAI_API_KEY
//
// The wrapper exists because yaml-v3 does not surface a single-key
// inline form cleanly enough; explicit valueFrom keeps the
// rendered docs unambiguous.
type ValueRef struct {
	ValueFrom *ValueSource `json:"valueFrom,omitempty" yaml:"valueFrom,omitempty"`
}

// Validate enforces "exactly one source set". Returns errdefs.Validation
// so the loader / CLI can surface the file:line location uniformly.
func (v ValueRef) Validate(field string) error {
	if v.ValueFrom == nil {
		return errdefs.Validationf("vesseld: %s.valueFrom is required (inline plain-text values are not allowed)", field)
	}
	set := 0
	if v.ValueFrom.Env != "" {
		set++
	}
	if v.ValueFrom.File != "" {
		set++
	}
	if v.ValueFrom.SecretRef != nil {
		set++
		if v.ValueFrom.SecretRef.Name == "" || v.ValueFrom.SecretRef.Key == "" {
			return errdefs.Validationf("vesseld: %s.valueFrom.secretRef requires both name and key", field)
		}
	}
	switch set {
	case 0:
		return errdefs.Validationf("vesseld: %s.valueFrom must set exactly one of env/file/secretRef", field)
	case 1:
		return nil
	default:
		return errdefs.Validationf("vesseld: %s.valueFrom must set exactly one of env/file/secretRef (got %d)", field, set)
	}
}

// Object is the interface every wire-level kind implements. Keeps
// the decode router and validator generic across kinds.
type Object interface {
	GetTypeMeta() TypeMeta
	GetObjectMeta() ObjectMeta
	Validate() error
}
