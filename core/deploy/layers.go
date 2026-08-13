package deploy

import (
	"context"
	"encoding/json"
	"io/fs"
	"sort"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// Layer is one configuration layer. Layers are merged in ascending
// Priority order, so a higher priority layer overrides a lower one.
// The lowest-priority layer must be a complete document (with
// version); later layers may be partial.
type Layer struct {
	Priority int
	Name     string
	Source   resource.Source
	BaseDir  string
	Embed    fs.FS
}

// LayerRef identifies the layer that provided a merged key.
type LayerRef struct {
	Name     string `json:"name,omitempty"`
	Priority int    `json:"priority"`
}

// Provenance records which layer provided each top-level resource and
// agent in the merged document: the highest-priority layer that
// declared the key.
type Provenance struct {
	Resources map[string]LayerRef `json:"resources"`
	Agents    map[string]LayerRef `json:"agents"`
}

// LoadLayers loads every layer with its own loader (file refs resolve
// against that layer's BaseDir), merges them in ascending priority
// order, and returns the merged document plus provenance.
func LoadLayers(ctx context.Context, layers []Layer) (Document, Provenance, error) {
	provenance := Provenance{
		Resources: make(map[string]LayerRef),
		Agents:    make(map[string]LayerRef),
	}
	if len(layers) == 0 {
		return Document{}, provenance, errdefs.Validationf(
			"deploy: at least one configuration layer is required")
	}
	sorted := append([]Layer(nil), layers...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})

	var merged Document
	for index, layer := range sorted {
		data, err := layer.load(ctx)
		if err != nil {
			return Document{}, provenance, errdefs.Validationf(
				"deploy: layer %d (%s): %v",
				layer.Priority, layer.Name, err)
		}
		partial, err := decodeLayer(data, index == 0)
		if err != nil {
			return Document{}, provenance, errdefs.Validationf(
				"deploy: layer %d (%s): %v",
				layer.Priority, layer.Name, err)
		}
		merged = mergeDocument(merged, partial)
		ref := LayerRef{Name: layer.Name, Priority: layer.Priority}
		for name := range partial.Resources {
			provenance.Resources[name] = ref
		}
		for name := range partial.Agents {
			provenance.Agents[name] = ref
		}
	}
	if err := merged.Validate(); err != nil {
		return Document{}, provenance, errdefs.Validationf(
			"deploy: merged document: %v", err)
	}
	return merged, provenance, nil
}

func (l Layer) load(ctx context.Context) ([]byte, error) {
	options := []resource.LoaderOption{
		resource.WithBaseDir(l.BaseDir),
	}
	if l.Embed != nil {
		options = append(options, resource.WithEmbed(l.Embed))
	}
	return resource.NewLoader(options...).Load(ctx, l.Source)
}

func decodeLayer(data []byte, requireVersion bool) (Document, error) {
	doc, err := utils.Decode[Document](data)
	if err != nil {
		return Document{}, err
	}
	if requireVersion {
		if err := doc.Validate(); err != nil {
			return Document{}, err
		}
		return doc, nil
	}
	// Later layers are partial by design: they may omit version and
	// resource/agent fields that earlier layers supply. Strict decode
	// only; completeness is checked on the merged document.
	return doc, nil
}

func mergeDocument(base, override Document) Document {
	merged := base
	if override.Version != "" {
		merged.Version = override.Version
	}
	merged.Runtime = mergeRuntime(base.Runtime, override.Runtime)
	merged.Resources = mergeResources(base.Resources, override.Resources)
	merged.Agents = mergeAgents(base.Agents, override.Agents)
	return merged
}

func mergeRuntime(base, override *resource.Opaque) *resource.Opaque {
	if override == nil {
		return base
	}
	if base == nil {
		return override
	}
	merged := resource.Opaque(mergeJSON(
		json.RawMessage(*base),
		json.RawMessage(*override),
	))
	return &merged
}

func mergeResources(base, override resource.Resources) resource.Resources {
	out := make(resource.Resources, len(base)+len(override))
	for name, res := range base {
		out[name] = res
	}
	for name, res := range override {
		if existing, ok := out[name]; ok {
			out[name] = mergeResource(existing, res)
		} else {
			out[name] = res
		}
	}
	return out
}

func mergeResource(base, override resource.Resource) resource.Resource {
	merged := base
	if override.Kind != "" {
		merged.Kind = override.Kind
	}
	if override.Impl != "" {
		merged.Impl = override.Impl
	}
	merged.Deps = mergeDeps(base.Deps, override.Deps)
	merged.Settings = mergeJSON(base.Settings, override.Settings)
	return merged
}

func mergeDeps(base, override resource.Deps) resource.Deps {
	out := make(resource.Deps, len(base)+len(override))
	for name, ref := range base {
		out[name] = ref
	}
	for name, ref := range override {
		out[name] = ref
	}
	return out
}

func mergeAgents(base, override map[string]agent.Definition) map[string]agent.Definition {
	out := make(map[string]agent.Definition, len(base)+len(override))
	for name, def := range base {
		out[name] = def
	}
	for name, def := range override {
		if existing, ok := out[name]; ok {
			out[name] = mergeAgent(existing, def)
		} else {
			out[name] = def
		}
	}
	return out
}

func mergeAgent(base, override agent.Definition) agent.Definition {
	merged := base
	merged.Card = mergeAgentCard(base.Card, override.Card)
	if override.Tools != nil {
		merged.Tools = override.Tools
	}
	merged.Policy = mergePolicy(base.Policy, override.Policy)
	merged.Engine = mergeEngineRef(base.Engine, override.Engine)
	if override.Prepare != nil {
		merged.Prepare = override.Prepare
	}
	if override.Observe != nil {
		merged.Observe = override.Observe
	}
	if override.Referees != nil {
		merged.Referees = override.Referees
	}
	if override.Commit != nil {
		merged.Commit = override.Commit
	}
	return merged
}

func mergePolicy(base, override *agent.Policy) *agent.Policy {
	if override == nil {
		return base
	}
	if base == nil {
		policy := *override
		return &policy
	}
	policy := *base
	if override.MaxRevise != 0 {
		policy.MaxRevise = override.MaxRevise
	}
	if override.ArtifactChannels != nil {
		policy.ArtifactChannels = append([]string(nil), override.ArtifactChannels...)
	}
	return &policy
}

func mergeAgentCard(base, override agent.AgentCard) agent.AgentCard {
	merged := base
	if override.Name != "" {
		merged.Name = override.Name
	}
	if override.Description != "" {
		merged.Description = override.Description
	}
	if override.Skills != nil {
		merged.Skills = override.Skills
	}
	if override.DefaultInputModes != nil {
		merged.DefaultInputModes = override.DefaultInputModes
	}
	if override.DefaultOutputModes != nil {
		merged.DefaultOutputModes = override.DefaultOutputModes
	}
	if override.Capabilities != (agent.AgentCapabilities{}) {
		merged.Capabilities = override.Capabilities
	}
	return merged
}

func mergeEngineRef(base, override agent.EngineRef) agent.EngineRef {
	merged := base
	if override.Kind != "" {
		merged.Kind = override.Kind
	}
	if override.Impl != "" {
		merged.Impl = override.Impl
	}
	merged.Deps = mergeDeps(base.Deps, override.Deps)
	merged.Settings = mergeJSON(base.Settings, override.Settings)
	return merged
}

// mergeJSON deep-merges two JSON values: objects merge recursively,
// arrays and scalars are replaced wholesale. nil/empty override keeps
// the base.
func mergeJSON(base, override json.RawMessage) json.RawMessage {
	if len(override) == 0 {
		return base
	}
	if len(base) == 0 {
		return override
	}
	var baseValue, overrideValue any
	if err := json.Unmarshal(base, &baseValue); err != nil {
		return override
	}
	if err := json.Unmarshal(override, &overrideValue); err != nil {
		return override
	}
	baseMap, baseOK := baseValue.(map[string]any)
	overrideMap, overrideOK := overrideValue.(map[string]any)
	if !baseOK || !overrideOK {
		return override
	}
	merged := mergeMaps(baseMap, overrideMap)
	out, err := json.Marshal(merged)
	if err != nil {
		return override
	}
	return out
}

func mergeMaps(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		baseValue, baseOK := out[key].(map[string]any)
		overrideValue, overrideOK := value.(map[string]any)
		if baseOK && overrideOK {
			out[key] = mergeMaps(baseValue, overrideValue)
		} else {
			out[key] = value
		}
	}
	return out
}
