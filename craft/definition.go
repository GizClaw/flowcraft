package craft

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils"
	"github.com/GizClaw/flowcraft/craft/internal/version"
)

// Version is the craft module version. Release builds override it with
// -ldflags; "0.0.0" marks a development build, where min_host_version
// checks are skipped.
var Version = "0.0.0"

// Definition is a decoded craft.yaml.
type Definition struct {
	Craft      Meta             `json:"craft"`
	UI         *UI              `json:"ui,omitempty"`
	Plugins    *Plugins         `json:"plugins,omitempty"`
	HostTools  *HostTools       `json:"host_tools,omitempty"`
	Deploy     *deploy.Document `json:"deploy,omitempty"`
	BaseLayers []BaseLayer      `json:"base_layers,omitempty"`
}

// Meta identifies the application and its compatibility floor.
type Meta struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Version        string `json:"version"`
	MinHostVersion string `json:"min_host_version,omitempty"`
}

// UI declares the host application's own frontend delivery. It is not
// the plugin UI channel.
type UI struct {
	Assets string   `json:"assets,omitempty"`
	Entry  string   `json:"entry,omitempty"`
	Kinds  []string `json:"kinds,omitempty"`
}

// Plugins declares plugin roots and mounting points.
type Plugins struct {
	Builtin      string   `json:"builtin,omitempty"`
	Roots        []string `json:"roots,omitempty"`
	ToolRegistry string   `json:"tool_registry,omitempty"`
	NodeTargets  []string `json:"node_targets,omitempty"`
}

// HostTools configures the built-in host primitive set.
type HostTools struct {
	Disable  []string                  `json:"disable,omitempty"`
	Services map[string]ServiceBinding `json:"services,omitempty"`
}

// ServiceBinding selects a named implementation for one primitive
// family.
type ServiceBinding struct {
	Impl string `json:"impl"`
}

// BaseLayer is one definition-level layer.
type BaseLayer struct {
	Name     string `json:"name,omitempty"`
	Priority int    `json:"priority,omitempty"`
	File     string `json:"file,omitempty"`
	Embed    string `json:"embed,omitempty"`
}

// ParseDefinition decodes and validates one craft.yaml (YAML or JSON).
func ParseDefinition(data []byte) (Definition, error) {
	def, err := utils.Decode[Definition](data)
	if err != nil {
		return Definition{}, errdefs.Validationf("craft: parse definition: %v", err)
	}
	if err := def.Validate(); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// Validate checks the static invariants of a definition.
func (d Definition) Validate() error {
	if strings.TrimSpace(d.Craft.ID) == "" {
		return errdefs.Validationf("craft: definition id is required")
	}
	if strings.TrimSpace(d.Craft.Version) == "" {
		return errdefs.Validationf("craft: definition version is required")
	}
	if d.Deploy != nil && len(d.BaseLayers) > 0 {
		return errdefs.Validationf(
			"craft: deploy and base_layers are mutually exclusive")
	}
	if d.Deploy == nil && len(d.BaseLayers) == 0 {
		return errdefs.Validationf(
			"craft: definition requires deploy or base_layers")
	}
	for i, layer := range d.BaseLayers {
		if layer.File != "" && layer.Embed != "" {
			return errdefs.Validationf(
				"craft: base_layers[%d] declares both file and embed", i)
		}
		if layer.File == "" && layer.Embed == "" {
			return errdefs.Validationf(
				"craft: base_layers[%d] requires file or embed", i)
		}
	}
	if min := strings.TrimSpace(d.Craft.MinHostVersion); min != "" {
		if !version.Valid(min) {
			return errdefs.Validationf(
				"craft: min_host_version %q is not a valid version", min)
		}
		if Version != "0.0.0" && version.Compare(Version, min) < 0 {
			return errdefs.Validationf(
				"craft: requires host version >= %s, running %s", min, Version)
		}
	}
	return nil
}
