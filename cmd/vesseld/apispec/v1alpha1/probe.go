package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Probe is the wire form of a health-check probe instance. Probe
// implementations live in the Catalog under names like
// "llm-reachable"; this document just packages the ref + opaque
// config and gives it a metadata.name so vessels can list it.
type Probe struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ProbeSpec `json:"spec" yaml:"spec"`
}

type ProbeSpec struct {
	// Ref is the Catalog probe-factory id. Required.
	Ref string `json:"ref" yaml:"ref"`

	// Config is the probe-specific config map (e.g. which
	// LLMProfile to ping, what message to send). Opaque to
	// apispec; the factory validates structure.
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

func (p Probe) GetTypeMeta() TypeMeta     { return p.TypeMeta }
func (p Probe) GetObjectMeta() ObjectMeta { return p.ObjectMeta }

func (p Probe) Validate() error {
	if err := p.ObjectMeta.Validate(KindProbe); err != nil {
		return err
	}
	if p.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld Probe %q: apiVersion %q != %q", p.Name, p.TypeMeta.APIVersion, APIVersion)
	}
	if p.TypeMeta.Kind != KindProbe {
		return errdefs.Validationf("vesseld Probe %q: kind %q != %q", p.Name, p.TypeMeta.Kind, KindProbe)
	}
	if p.Spec.Ref == "" {
		return errdefs.Validationf("vesseld Probe %q: spec.ref is required", p.Name)
	}
	return nil
}
