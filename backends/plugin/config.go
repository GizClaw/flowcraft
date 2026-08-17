package plugin

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils"
)

// PluginsConfig is the top-level plugins section of a deployment
// document, strictly decoded by the loader.
type PluginsConfig struct {
	// Dirs lists directories scanned for plugin directories.
	Dirs []string `json:"dirs"`
	// Enabled is an optional explicit whitelist of "name@constraint"
	// entries. Plugins load only when listed here: an empty list
	// enables nothing (explicit-enable semantics).
	Enabled []string `json:"enabled,omitempty"`
}

// ParsePluginsSection strictly decodes the plugins section of a
// deployment document (JSON or YAML).
func ParsePluginsSection(data []byte) (PluginsConfig, error) {
	config, err := utils.Decode[PluginsConfig](data)
	if err != nil {
		return PluginsConfig{}, errdefs.Validationf(
			"plugin: plugins section: %v", err)
	}
	if err := validatePluginsConfig(config); err != nil {
		return PluginsConfig{}, err
	}
	return config, nil
}

func validatePluginsConfig(config PluginsConfig) error {
	if len(config.Dirs) == 0 {
		return errdefs.Validationf(
			"plugin: plugins.dirs must list at least one directory")
	}
	for i, dir := range config.Dirs {
		if strings.TrimSpace(dir) == "" {
			return errdefs.Validationf("plugin: plugins.dirs[%d] is empty", i)
		}
	}
	for i, entry := range config.Enabled {
		if _, err := parseNamedConstraint(entry); err != nil {
			return errdefs.Validationf(
				"plugin: plugins.enabled[%d]: %v", i, err)
		}
	}
	return nil
}
