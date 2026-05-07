package v1alpha1

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// LLMProfile is the wire form of an LLM client configuration. The
// daemon instantiates one client per LLMProfile name and shares it
// across every Agent that references the profile, so the
// connection pool / rate-limit state is shared in-process.
//
// Provider is a Catalog ref (e.g. "openai", "anthropic"). Config
// is opaque to apispec; the provider factory decides what fields
// it accepts. Auth is the only nested struct because secret
// handling deserves a stable shape across providers.
type LLMProfile struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       LLMProfileSpec `json:"spec" yaml:"spec"`
}

type LLMProfileSpec struct {
	// Provider is the Catalog provider id ("openai", "anthropic",
	// "deepseek", "minimax", "bytedance", ...). Required.
	Provider string `json:"provider" yaml:"provider"`

	// Config is the provider-specific config (baseURL,
	// defaultModel, timeout, organisation id, etc.). Opaque to
	// apispec; validated by the provider factory at resolve time.
	Config map[string]any `json:"config,omitempty" yaml:"config,omitempty"`

	// Auth is mandatory for every provider in v0.1.0 (every
	// supported provider requires an api key). Even
	// passwordless local-llm setups must pass an empty string
	// via valueFrom.env to make the omission explicit.
	Auth LLMProfileAuth `json:"auth" yaml:"auth"`
}

// LLMProfileAuth currently only models APIKey because every v0.1.0
// provider authenticates with a single bearer-style key. When a
// provider needs OAuth / mTLS / signed-request credentials we add
// optional fields; existing configs remain valid because the new
// fields would be optional.
type LLMProfileAuth struct {
	APIKey ValueRef `json:"apiKey" yaml:"apiKey"`
}

func (l LLMProfile) GetTypeMeta() TypeMeta     { return l.TypeMeta }
func (l LLMProfile) GetObjectMeta() ObjectMeta { return l.ObjectMeta }

func (l LLMProfile) Validate() error {
	if err := l.ObjectMeta.Validate(KindLLMProfile); err != nil {
		return err
	}
	if l.TypeMeta.APIVersion != APIVersion {
		return errdefs.Validationf("vesseld LLMProfile %q: apiVersion %q != %q", l.Name, l.TypeMeta.APIVersion, APIVersion)
	}
	if l.TypeMeta.Kind != KindLLMProfile {
		return errdefs.Validationf("vesseld LLMProfile %q: kind %q != %q", l.Name, l.TypeMeta.Kind, KindLLMProfile)
	}
	if l.Spec.Provider == "" {
		return errdefs.Validationf("vesseld LLMProfile %q: spec.provider is required", l.Name)
	}
	if err := l.Spec.Auth.APIKey.Validate("LLMProfile " + l.Name + ".spec.auth.apiKey"); err != nil {
		return err
	}
	return nil
}
