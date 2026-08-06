package qwen

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/inference/config"
)

// defaultBaseURL is the Alibaba Model Studio (DashScope) Beijing endpoint.
// Workspace-dedicated domains ({} replaced by the workspace ID, e.g.
// https://ws-xxx.cn-beijing.maas.aliyuncs.com) and the international
// endpoints are set through base_url.
const defaultBaseURL = "https://dashscope.aliyuncs.com"

// SecretAPIKey is the DashScope API key.
const SecretAPIKey = "api_key"

// Spec is the provider-level configuration surface. It is credential-free:
// secrets resolve per profile and never appear here.
type Spec struct {
	// BaseURL overrides the API root. Defaults to
	// https://dashscope.aliyuncs.com; workspace-dedicated and regional
	// domains plug in here (without the /api/v1 suffix).
	BaseURL string `json:"base_url,omitempty"`
	// Models declares models outside the built-in catalog or overrides
	// catalog entries by name.
	Models []ModelSpec `json:"models,omitempty"`
}

// ModelSpec declares one model the deployment serves.
type ModelSpec struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Vision    bool   `json:"vision,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
}

// ProfileSpec carries per-profile overrides; currently empty.
type ProfileSpec struct{}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (m ModelSpec) Validate() error {
	if !modelNamePattern.MatchString(m.Name) {
		return fmt.Errorf("invalid model name %q", m.Name)
	}
	switch modelKind(m.Kind) {
	case "", kindGenerate, kindEmbed:
	default:
		return fmt.Errorf("model %q declares unsupported kind %q", m.Name, m.Kind)
	}
	return nil
}

func (s Spec) Validate() error {
	if s.BaseURL != "" {
		if err := validateURL("base_url", s.BaseURL); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(s.Models))
	for _, model := range s.Models {
		if err := model.Validate(); err != nil {
			return err
		}
		if seen[model.Name] {
			return fmt.Errorf("duplicate model %q", model.Name)
		}
		seen[model.Name] = true
	}
	return nil
}

func (ProfileSpec) Validate() error { return nil }

func validateURL(field, value string) error {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return fmt.Errorf("%s must be an http(s) URL, got %q", field, value)
	}
	return nil
}

func decodeSpec(raw []byte) (Spec, error) {
	spec, err := config.DecodeSpec[Spec](raw)
	if err != nil {
		return Spec{}, err
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func decodeProfileSpec(raw []byte) (ProfileSpec, error) {
	spec, err := config.DecodeSpec[ProfileSpec](raw)
	if err != nil {
		return ProfileSpec{}, err
	}
	if err := spec.Validate(); err != nil {
		return ProfileSpec{}, err
	}
	return spec, nil
}

// apiBase resolves the API root used to build endpoint paths.
func (s Spec) apiBase() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return defaultBaseURL
}
