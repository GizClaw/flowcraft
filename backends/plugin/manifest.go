package plugin

import (
	"regexp"
	"strings"

	"github.com/GizClaw/flowcraft/backends/plugin/service"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// namePattern is a reverse-domain plugin name: dot-separated lowercase
// alphanumeric labels, hyphens allowed inside a label.
var namePattern = regexp.MustCompile(
	`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$`)

// Manifest is the static description of a plugin directory, strictly
// decoded from plugin.yaml.
type Manifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description,omitempty"`
	Requires    Requires        `json:"requires,omitempty"`
	Provides    []resource.Spec `json:"provides,omitempty"`
	Artifacts   []Artifact      `json:"artifacts,omitempty"`
}

// Requires declares the plugin's host- and plugin-level dependencies.
type Requires struct {
	// Core is a semver constraint on the host core protocol version,
	// e.g. ">=0.4.0". It is checked at load time only; runtime
	// negotiation belongs to the RPC protocol version.
	Core string `json:"core,omitempty"`
	// Plugins lists "name@constraint" dependencies on other plugins.
	Plugins []string `json:"plugins,omitempty"`
}

// ArtifactType enumerates the supported artifact kinds.
type ArtifactType string

const (
	// ArtifactLayer is a declaration-layer artifact: a deploy.Layer
	// fragment merged into the deployment document.
	ArtifactLayer ArtifactType = "layer"
	// ArtifactService is an RPC service artifact (transport, command,
	// capabilities). The RPC channel consumes it.
	ArtifactService ArtifactType = "service"
	// ArtifactWASM is reserved for a future WASM compute slot. The
	// loader rejects it until the slot is implemented.
	ArtifactWASM ArtifactType = "wasm"
)

// Artifact is one contributed artifact. Fields are interpreted per
// Type; strict decoding rejects unknown fields and validation enforces
// the per-type invariants.
type Artifact struct {
	Type     string `json:"type"`
	Path     string `json:"path,omitempty"` // layer
	Priority int    `json:"priority,omitempty"`

	// Service fields.
	Transport       string            `json:"transport,omitempty"` // stdio | http
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	ProtocolVersion int               `json:"protocol_version,omitempty"`
	Capabilities    []resource.Spec   `json:"capabilities,omitempty"`
}

// ParseManifest strictly decodes a plugin.yaml document (JSON or YAML)
// and validates the static manifest fields. Layer file contents are
// validated by the Loader, which has the plugin directory context.
func ParseManifest(data []byte) (Manifest, error) {
	manifest, err := utils.Decode[Manifest](data)
	if err != nil {
		return Manifest{}, errdefs.Validationf("plugin: manifest: %v", err)
	}
	if err := validateManifestFields(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateManifestFields(m Manifest) error {
	if err := validateName(m.Name); err != nil {
		return err
	}
	if _, err := normalizeVersion(m.Version); err != nil {
		return errdefs.Validationf(
			"plugin %s: version %q: %v", m.Name, m.Version, err)
	}
	if m.Requires.Core != "" {
		if _, err := parseConstraint(m.Requires.Core); err != nil {
			return errdefs.Validationf(
				"plugin %s: requires.core %q: %v", m.Name, m.Requires.Core, err)
		}
	}
	for i, raw := range m.Requires.Plugins {
		if _, err := parseNamedConstraint(raw); err != nil {
			return errdefs.Validationf(
				"plugin %s: requires.plugins[%d] %q: %v", m.Name, i, raw, err)
		}
	}
	seen := make(map[resource.Key]struct{})
	for i, spec := range m.Provides {
		if err := spec.Validate(); err != nil {
			return errdefs.Validationf(
				"plugin %s: provides[%d]: %v", m.Name, i, err)
		}
		key := resource.Key{Kind: spec.Kind, Impl: spec.Impl}
		if _, dup := seen[key]; dup {
			return errdefs.Validationf(
				"plugin %s: duplicate provides %s/%s", m.Name, spec.Kind, spec.Impl)
		}
		seen[key] = struct{}{}
	}
	// provides and service capabilities may mirror each other (the
	// manifest declares the plugin-wide capability; the artifact
	// declares the same one for the RPC slot), so each list is
	// deduplicated separately.
	seenCapabilities := make(map[resource.Key]struct{})
	for _, artifact := range m.Artifacts {
		for i, spec := range artifact.Capabilities {
			if err := spec.Validate(); err != nil {
				return errdefs.Validationf(
					"plugin %s: service artifact capabilities[%d]: %v",
					m.Name, i, err)
			}
			key := resource.Key{Kind: spec.Kind, Impl: spec.Impl}
			if _, dup := seenCapabilities[key]; dup {
				return errdefs.Validationf(
					"plugin %s: duplicate service capability %s/%s",
					m.Name, spec.Kind, spec.Impl)
			}
			seenCapabilities[key] = struct{}{}
		}
	}
	for i, artifact := range m.Artifacts {
		if err := validateArtifact(m.Name, artifact); err != nil {
			return errdefs.Validationf(
				"plugin %s: artifacts[%d]: %v", m.Name, i, err)
		}
	}
	return nil
}

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return errdefs.Validationf(
			"plugin: invalid name %q: must be a lowercase reverse-domain name (e.g. acme.notion-tools)",
			name)
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 {
			return errdefs.Validationf(
				"plugin: invalid name %q: label %q exceeds 63 characters", name, label)
		}
	}
	return nil
}

func validateArtifact(pluginName string, a Artifact) error {
	switch ArtifactType(a.Type) {
	case ArtifactLayer:
		if a.Path == "" {
			return errdefs.Validationf(
				"plugin %s: layer artifact: path is required", pluginName)
		}
	case ArtifactService:
		switch a.Transport {
		case "stdio":
			if a.Command == "" {
				return errdefs.Validationf(
					"plugin %s: service artifact: stdio transport requires command", pluginName)
			}
		case "http":
			if a.URL == "" {
				return errdefs.Validationf(
					"plugin %s: service artifact: http transport requires url", pluginName)
			}
		case "":
			return errdefs.Validationf(
				"plugin %s: service artifact: transport is required (stdio | http)", pluginName)
		default:
			return errdefs.Validationf(
				"plugin %s: service artifact: unknown transport %q", pluginName, a.Transport)
		}
		if a.ProtocolVersion != 0 &&
			a.ProtocolVersion != service.ProtocolVersion1 {
			return errdefs.Validationf(
				"plugin %s: service artifact: protocol_version %d is not supported "+
					"(want %d)",
				pluginName, a.ProtocolVersion, service.ProtocolVersion1)
		}
		for i, spec := range a.Capabilities {
			if err := spec.Validate(); err != nil {
				return errdefs.Validationf(
					"plugin %s: service artifact: capabilities[%d]: %v", pluginName, i, err)
			}
		}
	case ArtifactWASM:
		return errdefs.Validationf(
			"plugin %s: artifact type %q is reserved and not implemented", pluginName, a.Type)
	case "":
		return errdefs.Validationf("plugin %s: artifact type is required", pluginName)
	default:
		return errdefs.Validationf(
			"plugin %s: unknown artifact type %q", pluginName, a.Type)
	}
	return nil
}
