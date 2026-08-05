// Package derive executes deterministic typed write-side derivation DAGs.
package derive

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/component"
)

// NodeSpec selects a Deriver and names its direct dependencies. DependsOn order
// also defines the order in which dependency artifacts are presented.
type NodeSpec struct {
	ID        string                `json:"id"`
	Deriver   component.DeriverSpec `json:"-"`
	DependsOn []string              `json:"depends_on,omitempty"`
}

// Spec describes only write-side derivation. It is not a general graph runtime.
type Spec struct {
	SourceKinds []component.ArtifactKind `json:"source_kinds,omitempty"`
	Nodes       []NodeSpec               `json:"nodes"`
}

type node struct {
	spec    NodeSpec
	deriver component.Deriver
	factory component.FactoryMetadata
}

// DAG is an immutable, deterministic write-side derivation plan.
type DAG struct {
	spec  Spec
	order []string
	nodes map[string]node
}

// Build validates a DAG and resolves every Deriver before returning it.
func Build(registry *component.Registry, spec Spec) (*DAG, error) {
	if registry == nil {
		return nil, errors.New("memory derive: component registry is required")
	}
	cloned := cloneSpec(spec)
	order, err := validateAndOrder(cloned)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]node, len(cloned.Nodes))
	byID := make(map[string]NodeSpec, len(cloned.Nodes))
	for _, item := range cloned.Nodes {
		byID[item.ID] = item
	}
	for _, id := range order {
		item := byID[id]
		deriver, metadata, err := registry.ResolveTypedDeriver(item.Deriver)
		if err != nil {
			return nil, fmt.Errorf("memory derive: node %q: %w", id, err)
		}
		nodes[id] = node{spec: item, deriver: deriver, factory: metadata}
	}
	if err := validatePorts(cloned, nodes); err != nil {
		return nil, err
	}
	return &DAG{spec: cloned, order: order, nodes: nodes}, nil
}

func validatePorts(spec Spec, nodes map[string]node) error {
	for _, item := range spec.Nodes {
		current := nodes[item.ID]
		inputs := spec.SourceKinds
		if len(item.DependsOn) > 0 {
			inputs = nil
			for _, dependency := range item.DependsOn {
				inputs = append(inputs, nodes[dependency].factory.Ports.Outputs...)
			}
		}
		accepted := current.factory.Ports.Inputs
		if len(inputs) == 0 || len(accepted) == 0 {
			continue
		}
		for _, kind := range inputs {
			if !containsKind(accepted, kind) {
				return fmt.Errorf(
					"memory derive: node %q input kind %q is incompatible with factory %q inputs %v",
					item.ID, kind, current.factory.Name, accepted,
				)
			}
		}
	}
	return nil
}

func containsKind(kinds []component.ArtifactKind, want component.ArtifactKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

// Spec returns an independently owned DAG specification.
func (dag *DAG) Spec() Spec {
	if dag == nil {
		return Spec{}
	}
	return cloneSpec(dag.spec)
}

// TopologicalOrder returns the stable execution order.
func (dag *DAG) TopologicalOrder() []string {
	if dag == nil {
		return nil
	}
	return append([]string(nil), dag.order...)
}

func validateAndOrder(spec Spec) ([]string, error) {
	if len(spec.Nodes) == 0 {
		return nil, errors.New("memory derive: nodes are required")
	}
	byID := make(map[string]NodeSpec, len(spec.Nodes))
	for index, item := range spec.Nodes {
		if strings.TrimSpace(item.ID) == "" {
			return nil, fmt.Errorf("memory derive: node %d id is required", index)
		}
		if _, exists := byID[item.ID]; exists {
			return nil, fmt.Errorf("memory derive: duplicate node id %q", item.ID)
		}
		byID[item.ID] = item
	}

	indegree := make(map[string]int, len(spec.Nodes))
	dependents := make(map[string][]string, len(spec.Nodes))
	for _, item := range spec.Nodes {
		seen := make(map[string]struct{}, len(item.DependsOn))
		for _, dependency := range item.DependsOn {
			if _, duplicate := seen[dependency]; duplicate {
				return nil, fmt.Errorf("memory derive: node %q has duplicate dependency %q", item.ID, dependency)
			}
			seen[dependency] = struct{}{}
			if dependency == item.ID {
				return nil, fmt.Errorf("memory derive: node %q depends on itself", item.ID)
			}
			if _, exists := byID[dependency]; !exists {
				return nil, fmt.Errorf("memory derive: node %q dependency %q is missing", item.ID, dependency)
			}
			indegree[item.ID]++
			dependents[dependency] = append(dependents[dependency], item.ID)
		}
	}
	for id := range byID {
		sort.Strings(dependents[id])
	}

	ready := make([]string, 0)
	for id := range byID {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(spec.Nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = insertSorted(ready, dependent)
			}
		}
	}
	if len(order) != len(spec.Nodes) {
		return nil, errors.New("memory derive: dependency cycle detected")
	}
	return order, nil
}

func insertSorted(values []string, value string) []string {
	index := sort.SearchStrings(values, value)
	values = append(values, "")
	copy(values[index+1:], values[index:])
	values[index] = value
	return values
}

func cloneSpec(spec Spec) Spec {
	if spec.Nodes == nil {
		return Spec{}
	}
	cloned := Spec{
		SourceKinds: append([]component.ArtifactKind(nil), spec.SourceKinds...),
		Nodes:       make([]NodeSpec, len(spec.Nodes)),
	}
	for index, item := range spec.Nodes {
		item.DependsOn = append([]string(nil), item.DependsOn...)
		cloned.Nodes[index] = item
	}
	return cloned
}
