package config

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"

	"go.yaml.in/yaml/v3"
)

// Requirement is what a set of layers needs from the environment.
//
// It exists because the alternative is discovering a missing variable one
// process restart at a time: expansion fails at the first unset reference, so a
// deployment with four required variables takes four starts to describe itself.
// A host can call [RequiredPlaceholders] first and report everything at once.
type Requirement struct {
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
func (e Requirement) Missing(lookup func(string) (string, bool)) []string {
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

// RequiredPlaceholders reports which environment variables the layers reference,
// without needing any of them to be set.
//
// It parses the same tree expansion parses and uses the same scanner, so a value
// inside a comment or inside a key is not a reference — only a string value is.
//
// It reads every layer's body, and a caller that then loads the stack reads them
// again: a [Func] source that fetches is asked twice, so a checklist and a load
// are two moments of one startup rather than two calls over one source.
func RequiredPlaceholders(ctx context.Context, layers ...Layer) (Requirement, error) {
	requirement := Requirement{Sources: make(map[string][]string)}
	required := make(map[string]struct{})
	optional := make(map[string]struct{})

	for _, layer := range layers {
		body, err := layer.body(ctx, defaultLabel)
		if err != nil {
			return Requirement{}, err
		}
		document, err := parseDocument(body)
		if err != nil {
			return Requirement{}, errdefs.Validationf(
				"%s: layer %q: %v", defaultLabel, layer.Name, err)
		}
		if document == nil {
			continue
		}
		if err := collectEnv(document, layer, &requirement, required, optional); err != nil {
			return Requirement{}, err
		}
	}

	return finishEnv(requirement, required, optional), nil
}

// finishEnv turns the accumulated name sets into the sorted report.
func finishEnv(
	requirement Requirement,
	required, optional map[string]struct{},
) Requirement {
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
	return requirement
}

// RequiredPlaceholdersDocument reports which environment variables one YAML
// document references, without resolving them.
//
// RequiredPlaceholders answers the same question for layers, which have names
// and are merged; a caller that owns a single document — a deployment document,
// for one — needs the answer before it can build a layer, and needs the source
// name to say which file to edit. The scan is the one expansion uses, so only
// string values count.
func RequiredPlaceholdersDocument(body []byte, source string) (Requirement, error) {
	requirement := Requirement{Sources: make(map[string][]string)}
	required := make(map[string]struct{})
	optional := make(map[string]struct{})
	document, err := parseDocument(body)
	if err != nil {
		return Requirement{}, errdefs.Validationf("%s: %v", source, err)
	}
	if document == nil {
		return finishEnv(requirement, required, optional), nil
	}
	if err := collectEnv(document, Layer{Name: source}, &requirement, required, optional); err != nil {
		return Requirement{}, err
	}
	return finishEnv(requirement, required, optional), nil
}

func collectEnv(
	node *yaml.Node,
	layer Layer,
	requirement *Requirement,
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
		segments, err := scanPlaceholders(node.Value, defaultLabel, "")
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
