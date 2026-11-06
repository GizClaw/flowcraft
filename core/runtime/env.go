package runtime

import (
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"go.yaml.in/yaml/v3"
)

// EnvRequirement is what a set of layers needs from the environment.
//
// It exists because the alternative is discovering a missing variable one
// process restart at a time: expansion fails at the first unset reference, so a
// deployment with four required variables takes four starts to describe itself.
// A host can call RequiredEnv first and report everything at once.
type EnvRequirement struct {
	// Required names references with no fallback: the process cannot start
	// without them.
	Required []string
	// Optional names references with a fallback: absent is fine.
	Optional []string
	// Sources maps a variable to the layers that reference it, so the error can
	// point at the file the reader should edit.
	Sources map[string][]string
}

// Missing reports the required variables the given environment does not define.
func (e EnvRequirement) Missing(lookup func(string) (string, bool)) []string {
	if lookup == nil {
		return nil
	}
	missing := make([]string, 0, len(e.Required))
	for _, name := range e.Required {
		if _, set := lookup(name); !set {
			missing = append(missing, name)
		}
	}
	return missing
}

// RequiredEnv reports which environment variables the layers reference, without
// needing any of them to be set.
//
// It parses the same tree expansion parses and uses the same scanner, so a value
// inside a comment or inside a key is not a reference — only a string value is.
func RequiredEnv(layers ...Layer) (EnvRequirement, error) {
	requirement := EnvRequirement{Sources: make(map[string][]string)}
	required := make(map[string]struct{})
	optional := make(map[string]struct{})

	for _, layer := range layers {
		body, err := layer.body()
		if err != nil {
			return EnvRequirement{}, err
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			return EnvRequirement{}, errdefs.Validationf(
				"runtime: layer %q: %v", layer.Name, err)
		}
		if err := collectEnv(&document, layer, &requirement, required, optional); err != nil {
			return EnvRequirement{}, err
		}
	}

	for name := range required {
		requirement.Required = append(requirement.Required, name)
	}
	for name := range optional {
		if _, isRequired := required[name]; isRequired {
			continue
		}
		requirement.Optional = append(requirement.Optional, name)
	}
	sort.Strings(requirement.Required)
	sort.Strings(requirement.Optional)
	return requirement, nil
}

func collectEnv(
	node *yaml.Node,
	layer Layer,
	requirement *EnvRequirement,
	required, optional map[string]struct{},
) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := collectEnv(child, layer, requirement, required, optional); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := collectEnv(node.Content[i+1], layer, requirement, required, optional); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return nil
		}
		segments, err := scanPlaceholders(node.Value, layer.Name)
		if err != nil {
			return err
		}
		for _, segment := range segments {
			if segment.scheme == "" {
				continue
			}
			name := segment.scheme + ":" + strings.Join(segment.args, ":")
			if segment.scheme == SchemeEnv && len(segment.args) == 1 {
				name = segment.args[0]
			}
			if segment.hasFallback {
				optional[name] = struct{}{}
			} else {
				required[name] = struct{}{}
			}
			sources := requirement.Sources[name]
			if len(sources) == 0 || sources[len(sources)-1] != layer.Name {
				requirement.Sources[name] = append(sources, layer.Name)
			}
		}
	}
	return nil
}
