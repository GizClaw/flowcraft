package craft

import (
	"io/fs"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/craft/hostmcp"
)

// Options configures one Craft instance.
type Options struct {
	// ConfigDir, DataDir and AppHome are the application roots. AppHome
	// defaults to DataDir; ConfigDir/DataDir may be empty when the
	// definition does not reference paths.
	ConfigDir string
	DataDir   string
	AppHome   string

	// DefinitionDir anchors definition-level file references
	// (base_layers with a file source). Empty means the process working
	// directory.
	DefinitionDir string

	// Assets resolves definition-level embed references (base_layers
	// with an embed source).
	Assets fs.FS

	// Loader, when set, replaces the default loader built from the
	// definition directory.
	Loader *resource.Loader

	// Capabilities are the compile-time extensions of this Craft.
	Capabilities []Capability

	// Plugins is the optional plugin host. When set, craft injects its
	// tool set and host into every runtime and reloads open runtimes on
	// plugin changes.
	Plugins PluginHost

	// HostServices supplies the behavior of the built-in host
	// primitives. Capabilities implementing HostServices are merged on
	// top of this value.
	HostServices hostmcp.Services

	// Externals are application-level values injected into every
	// runtime and declared automatically in runtime.external_deps.
	Externals []deploy.ExternalResource

	// Layers are extra craft-level layers (for example an explicit or
	// user layer supplied by the manager). They merge on top of the
	// definition layers and below every runtime-scoped layer.
	Layers []deploy.Layer

	// Values feed ${craft:NAME} references craft-wide; per-runtime
	// RuntimeOptions.Values override them.
	Values map[string]string
}

// RuntimeKey identifies one runtime inside a Craft. The meaning is
// application-defined: a workspace path, a profile name, a tenant id.
type RuntimeKey string

// DefaultKey is the conventional key for single-runtime applications.
const DefaultKey RuntimeKey = "default"

// RuntimeOptions carries the per-runtime inputs of OpenRuntime.
type RuntimeOptions struct {
	// Layers merge on top of every craft-level layer.
	Layers []deploy.Layer
	// Externals are values injected into this runtime only.
	Externals []deploy.ExternalResource
	// HostDecorators wrap the runtime's base host factory for this
	// runtime; craft-wide decorators come from capabilities.
	HostDecorators []runtime.HostFactoryDecorator
	// ResultHostDecorators wrap the host factory with access to the
	// assembled deployment for this runtime.
	ResultHostDecorators []runtime.ResultHostFactoryDecorator
	// Values feed ${craft:NAME} and the craft.runtime external for
	// this runtime, overriding craft-level values.
	Values map[string]string
}

// RuntimeContext is the value injected as the craft.runtime external
// into every runtime, so factories can adapt their construction to the
// runtime they are built for.
type RuntimeContext struct {
	Key       RuntimeKey        `json:"key"`
	Values    map[string]string `json:"values,omitempty"`
	ConfigDir string            `json:"config_dir,omitempty"`
	DataDir   string            `json:"data_dir,omitempty"`
	AppHome   string            `json:"app_home,omitempty"`
}

// RuntimeContextContract is the dependency contract of RuntimeContext.
const RuntimeContextContract = "craft.RuntimeContext"
