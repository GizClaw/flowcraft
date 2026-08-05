package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	"github.com/GizClaw/flowcraft/memory/lines/knowledge"
)

func normalizedAlgorithms(configured AlgorithmCatalog, factories *FactoryCatalog, selected map[string]struct{}) ([]Algorithm, error) {
	versions := map[string]string{
		"chat.fact":       chat.AlgorithmVersion,
		"knowledge.chunk": knowledge.AlgorithmVersion,
	}
	seenConfigured := make(map[string]struct{}, len(configured))
	for _, algorithm := range configured {
		if err := component.ValidateName(algorithm.Name); err != nil {
			return nil, fmt.Errorf("memory config: algorithm catalog: %w", err)
		}
		if strings.TrimSpace(algorithm.Version) == "" {
			return nil, fmt.Errorf("memory config: algorithm %q version is required", algorithm.Name)
		}
		if _, duplicate := seenConfigured[algorithm.Name]; duplicate {
			return nil, fmt.Errorf("memory config: duplicate algorithm %q", algorithm.Name)
		}
		seenConfigured[algorithm.Name] = struct{}{}
		if current, exists := versions[algorithm.Name]; exists && current != algorithm.Version {
			return nil, fmt.Errorf(
				"memory config: algorithm %q version %q conflicts with %q",
				algorithm.Name, algorithm.Version, current,
			)
		}
		versions[algorithm.Name] = algorithm.Version
	}
	for _, metadata := range factories.TypedDeriverMetadata() {
		if current, exists := versions[metadata.Name]; exists && current != metadata.Version {
			return nil, fmt.Errorf(
				"memory config: factory %q version %q conflicts with algorithm catalog %q",
				metadata.Name, metadata.Version, current,
			)
		}
		versions[metadata.Name] = metadata.Version
	}
	names := make([]string, 0, len(versions))
	for name := range versions {
		if _, used := selected[name]; used {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]Algorithm, len(names))
	for index, name := range names {
		result[index] = Algorithm{Name: name, Version: versions[name]}
	}
	for name := range selected {
		if _, exists := versions[name]; !exists {
			return nil, fmt.Errorf("memory config: selected algorithm %q has no version", name)
		}
	}
	return result, nil
}
