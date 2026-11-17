package config

import (
	"strings"

	"go.yaml.in/yaml/v3"
)

// pathSeparator joins the segments of one provenance path. It is not "." so a
// key that contains a dot keeps its meaning: provenance is now a public query
// surface, and a dotted key would make two different paths indistinguishable.
const pathSeparator = "\x1f"

// Provenance records which layer last supplied each value.
//
// It answers the only question a merged configuration invites: which file put
// this value here. Without it, a resolved write policy is a fact with no author.
type Provenance struct {
	entries map[string]provenanceEntry
	order   []string
}

type provenanceEntry struct {
	path []string
	ref  Ref
}

// Origin returns the layer that last set the value at path.
func (p Provenance) Origin(path ...string) (Ref, bool) {
	entry, found := p.entries[encodePath(path)]
	return entry.ref, found
}

// Each walks every recorded value in the order it was first supplied, so a
// report prints the same way on every run.
func (p Provenance) Each(yield func(path []string, ref Ref) bool) {
	for _, key := range p.order {
		entry := p.entries[key]
		if !yield(entry.path, entry.ref) {
			return
		}
	}
}

// Len is how many values were recorded.
func (p Provenance) Len() int { return len(p.entries) }

func (p *Provenance) record(path []string, ref Ref) {
	key := encodePath(path)
	if p.entries == nil {
		p.entries = make(map[string]provenanceEntry)
	}
	if _, exists := p.entries[key]; !exists {
		p.order = append(p.order, key)
	}
	p.entries[key] = provenanceEntry{path: append([]string(nil), path...), ref: ref}
}

func encodePath(path []string) string { return strings.Join(path, pathSeparator) }

// mergeNode merges over into base and returns the node to continue with.
//
// The three rules are fixed and stated in the package documentation: mapping
// merges key by key, a sequence is replaced whole, a scalar is replaced.
func mergeNode(base, over *yaml.Node, prov *Provenance, ref Ref, path []string) *yaml.Node {
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
		child := appendPath(path, key.Value)
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

// appendPath returns a new path; sharing the parent slice would let one branch's
// key appear in another branch's provenance.
func appendPath(prefix []string, key string) []string {
	path := make([]string, len(prefix)+1)
	copy(path, prefix)
	path[len(prefix)] = key
	return path
}

// recordAll marks every leaf under node as supplied by ref. A nil recorder — the
// load asked not to keep provenance — makes this a walk that keeps nothing: the
// merge still has to visit the node, so the check is per call, not per leaf.
func recordAll(prov *Provenance, node *yaml.Node, path []string, ref Ref) {
	if prov == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 1 {
			recordAll(prov, node.Content[0], path, ref)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			recordAll(prov, node.Content[i+1], appendPath(path, node.Content[i].Value), ref)
		}
	case yaml.SequenceNode:
		// A sequence is one value: the replacement rule means its items cannot
		// come from different layers, so the list itself is what is recorded.
		if len(path) > 0 {
			prov.record(path, ref)
		}
	default:
		if len(path) > 0 {
			prov.record(path, ref)
		}
	}
}
