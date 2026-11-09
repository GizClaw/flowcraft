package runtime

import (
	"context"
	"os"
	"sort"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/decode"

	"go.yaml.in/yaml/v3"
)

// Layer is one configuration layer. Layers merge in ascending priority, so a
// higher priority layer overrides a lower one.
//
// Priority is explicit rather than implied by order because the order a host
// receives layers in is not always the order it wants them applied: a profile
// picked on the command line belongs above a file listed in a directory scan,
// whatever the scan produced. Duplicate priorities are rejected instead of
// being broken by a tie-break nobody can predict.
type Layer struct {
	// Priority orders the layer; higher wins.
	Priority int
	// Name identifies the layer in errors and provenance.
	Name string
	// Path reads the layer from a file when Bytes is empty.
	Path string
	// Bytes is the layer body, for embedded or generated layers.
	Bytes []byte
}

// LayerRef identifies the layer that supplied a merged value.
type LayerRef struct {
	Priority int
	Name     string
}

// Provenance maps a dotted configuration path to the layer that last set it.
//
// It exists to answer the only question a merged configuration invites: which
// file put this value here. Without it, a resolved write policy is a fact with
// no author.
type Provenance map[string]LayerRef

// LoadLayers parses, merges and expands layers into one configuration.
//
// Placeholders are resolved once, after the merge: a value a higher layer
// overrides is never looked up. That matters because a resolver may read a
// secret store, log an access, or fail — resolving per layer would do that work
// for values nobody ends up using.
//
// Merge rules are per node kind and nothing is inferred by reflection:
//
//   - mapping: merged key by key, so a layer may add one field to a section;
//   - sequence: replaced whole, because appending makes "one more line" add a
//     writable path, and a permission list must not grow silently;
//   - scalar: replaced.
func LoadLayers(ctx context.Context, layers []Layer, opts ...LoadOption) (Config, Provenance, error) {
	if len(layers) == 0 {
		return Config{}, nil, errdefs.Validationf("runtime: at least one layer is required")
	}
	settings := loadOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	ordered := append([]Layer(nil), layers...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Priority == ordered[i-1].Priority {
			return Config{}, nil, errdefs.Validationf(
				"runtime: layers %q and %q share priority %d",
				ordered[i-1].Name, ordered[i].Name, ordered[i].Priority)
		}
	}

	provenance := Provenance{}
	var merged *yaml.Node
	for _, layer := range ordered {
		body, err := layer.body()
		if err != nil {
			return Config{}, nil, err
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			return Config{}, nil, errdefs.Validationf(
				"runtime: layer %q: %v", layer.Name, err)
		}
		ref := LayerRef{Priority: layer.Priority, Name: layer.Name}
		if merged == nil {
			merged = &document
			recordAll(provenance, merged, "", ref)
			continue
		}
		merged = mergeNode(merged, &document, provenance, ref, "")
	}
	if merged == nil || len(merged.Content) == 0 {
		return Config{}, nil, errdefs.Validationf("runtime: the layers describe an empty document")
	}
	if err := resolvePlaceholders(ctx, documentBody(merged), "", settings); err != nil {
		return Config{}, nil, err
	}

	cfg, err := decodeStrict(merged)
	if err != nil {
		return Config{}, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, nil, err
	}
	return cfg, provenance, nil
}

func (l Layer) body() ([]byte, error) {
	if len(l.Bytes) > 0 {
		return l.Bytes, nil
	}
	if l.Path == "" {
		return nil, errdefs.Validationf("runtime: layer %q has neither a path nor bytes", l.Name)
	}
	body, err := os.ReadFile(l.Path)
	if err != nil {
		return nil, errdefs.Validationf("runtime: layer %q: %w", l.Name, err)
	}
	return body, nil
}

// decodeStrict turns the merged tree into a Config, rejecting unknown keys: a
// typo in a permission section must not be silently ignored.
// decodeStrict turns the merged document into JSON and decodes it strictly.
//
// Merging happens on YAML nodes because that is where per-kind merge rules and
// provenance live; the document a caller writes is YAML, and the structure it
// becomes is JSON with unknown fields rejected.
func decodeStrict(node *yaml.Node) (Config, error) {
	body, err := yaml.Marshal(node)
	if err != nil {
		return Config{}, errdefs.Validationf("runtime: re-encode merged document: %v", err)
	}
	cfg, err := decode.Decode[Config](body)
	if err != nil {
		return Config{}, errdefs.Validationf("runtime configuration: %v", err)
	}
	return cfg, nil
}

// mergeNode merges over into base and returns the node to continue with.
func mergeNode(base, over *yaml.Node, prov Provenance, ref LayerRef, path string) *yaml.Node {
	if base.Kind == yaml.DocumentNode && len(base.Content) == 1 {
		base.Content[0] = mergeNode(base.Content[0], documentBody(over), prov, ref, path)
		return base
	}
	if over.Kind == yaml.DocumentNode {
		over = documentBody(over)
	}
	if base.Kind != yaml.MappingNode || over.Kind != yaml.MappingNode {
		recordAll(prov, over, path, ref)
		return over
	}
	for i := 0; i+1 < len(over.Content); i += 2 {
		key, value := over.Content[i], over.Content[i+1]
		child := joinPath(path, key.Value)
		replaced := false
		for j := 0; j+1 < len(base.Content); j += 2 {
			if base.Content[j].Value != key.Value {
				continue
			}
			if base.Content[j+1].Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
				base.Content[j+1] = mergeNode(base.Content[j+1], value, prov, ref, child)
			} else {
				base.Content[j+1] = value
				recordAll(prov, value, child, ref)
			}
			replaced = true
			break
		}
		if !replaced {
			base.Content = append(base.Content, key, value)
			recordAll(prov, value, child, ref)
		}
	}
	return base
}

func documentBody(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		return node.Content[0]
	}
	return node
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// recordAll marks every leaf under node as supplied by ref.
func recordAll(prov Provenance, node *yaml.Node, path string, ref LayerRef) {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 1 {
			recordAll(prov, node.Content[0], path, ref)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			recordAll(prov, node.Content[i+1], joinPath(path, node.Content[i].Value), ref)
		}
	case yaml.SequenceNode:
		if path != "" {
			prov[path] = ref
		}
	default:
		if path != "" {
			prov[path] = ref
		}
	}
}
