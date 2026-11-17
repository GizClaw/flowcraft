package config

import (
	"github.com/GizClaw/flowcraft/core/errdefs"

	"go.yaml.in/yaml/v3"
)

// expandMergeKeys replaces every `<<` in the tree with the entries it merges.
//
// The YAML decoder applies merge keys while it builds a value, but the merged
// tree is inspected before that: without this pass Doc.Raw would not find a key
// that Canonical renders, and provenance would report a path segment named "<<"
// that no reader can find in the document. Expanding once at parse time makes
// the tree literal, so every answer this package gives agrees by construction.
//
// The rules are YAML's: an explicit key wins over a merged one, and an earlier
// source wins over a later one. A source must be a mapping, or a sequence of
// mappings; anything else is refused rather than ignored, because a merge that
// merges nothing is a document whose author expected otherwise.
func expandMergeKeys(document *yaml.Node) error {
	return expandMergeNode(documentBody(document))
}

func expandMergeNode(node *yaml.Node) error {
	switch node.Kind {
	case yaml.AliasNode:
		// The anchored node is expanded where it is defined. Walking through the
		// alias would expand it twice and, on a recursive anchor, forever.
		return nil
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := expandMergeNode(child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		// Values first: a source mapping has to be expanded before it is copied,
		// so one document's expansion never depends on the parent's order.
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := expandMergeNode(node.Content[i+1]); err != nil {
				return err
			}
		}
		return mergeInto(node)
	}
	return nil
}

// mergeInto rewrites one mapping in place: explicit pairs first, then the pairs
// its merge keys contribute that nothing has claimed yet.
// nodePair is one key/value pair of a YAML mapping.
type nodePair struct{ key, value *yaml.Node }

func mergeInto(node *yaml.Node) error {
	explicit := make([]nodePair, 0, len(node.Content)/2)
	claimed := make(map[string]struct{}, len(node.Content)/2)
	mergedSources := make([]*yaml.Node, 0, 1)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag == "!!merge" {
			mergedSources = append(mergedSources, value)
			continue
		}
		explicit = append(explicit, nodePair{key, value})
		claimed[key.Value] = struct{}{}
	}
	if len(mergedSources) == 0 {
		return nil
	}

	merged := make([]nodePair, 0, len(explicit))
	for _, source := range mergedSources {
		pairs, err := mergeSourcePairs(source)
		if err != nil {
			return err
		}
		for _, candidate := range pairs {
			if _, taken := claimed[candidate.key.Value]; taken {
				continue
			}
			claimed[candidate.key.Value] = struct{}{}
			merged = append(merged, candidate)
		}
	}

	content := make([]*yaml.Node, 0, 2*(len(explicit)+len(merged)))
	for _, item := range explicit {
		content = append(content, item.key, item.value)
	}
	for _, item := range merged {
		content = append(content, item.key, item.value)
	}
	node.Content = content
	return nil
}

// mergeSourcePairs resolves one `<<` value: an alias to a mapping, a mapping, or
// a sequence of either, in declaration order.
func mergeSourcePairs(source *yaml.Node) ([]nodePair, error) {
	resolved, err := resolveMergeSource(source)
	if err != nil {
		return nil, err
	}
	if resolved.Kind == yaml.SequenceNode {
		pairs := make([]nodePair, 0, len(resolved.Content))
		for _, item := range resolved.Content {
			fromItem, err := mergeSourcePairs(item)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, fromItem...)
		}
		return pairs, nil
	}
	if resolved.Kind != yaml.MappingNode {
		return nil, errdefs.Validationf(
			"a `<<` merge source must be a mapping, or a sequence of mappings; got %s",
			nodeKindName(resolved))
	}
	return clonePairs(resolved.Content), nil
}

func resolveMergeSource(node *yaml.Node) (*yaml.Node, error) {
	if node.Kind != yaml.AliasNode {
		return node, nil
	}
	if node.Alias == nil {
		return nil, errdefs.Validationf("a `<<` merge source aliases nothing")
	}
	return node.Alias, nil
}

// clonePairs deep-copies the pairs of a source mapping. The copy matters: the
// source is often an anchored node that another document position also uses, and
// the merge appends into the target mapping — appending through a shared pointer
// would grow the anchor as a side effect.
func clonePairs(content []*yaml.Node) []nodePair {
	pairs := make([]nodePair, 0, len(content)/2)
	for i := 0; i+1 < len(content); i += 2 {
		pairs = append(pairs, nodePair{cloneNode(content[i]), cloneNode(content[i+1])})
	}
	return pairs
}

func cloneNode(node *yaml.Node) *yaml.Node {
	cloned := *node
	if len(node.Content) > 0 {
		cloned.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			cloned.Content[i] = cloneNode(child)
		}
	}
	return &cloned
}

// nodeKindName names a merge source in an error. The caller has already resolved
// aliases, so only the three kinds a `<<` value can hold are reachable.
func nodeKindName(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a sequence"
	default:
		return "the scalar " + node.Value
	}
}
