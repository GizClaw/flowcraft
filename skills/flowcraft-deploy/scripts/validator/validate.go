package main

import (
	"context"
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// validateDeploy loads a deployment document the way a host does: placeholders
// first (a structural check must not need real credentials), then the strict
// decode, the kind table, the runtime layers and their configuration.
func validateDeploy(path string) error {
	loader := deploy.Loader{Kinds: deploy.BuiltinKinds()}

	requirement, err := loader.RequiredPlaceholdersFile(path)
	if err != nil {
		return err
	}
	// The document's own references are filled with placeholders: whether the
	// value is a real key is the host's question at start time, not a structural
	// one. A reference with a fallback is already satisfiable.
	if err := placeholderEnv(requirement.Required); err != nil {
		return err
	}
	// A runtime layer is a file of its own, so its references are not in the
	// requirement above: read each layer the document names and fill those too,
	// or "validate without credentials" would only be true for the outer file.
	if err := placeholderLayers(path); err != nil {
		return err
	}

	specs, err := loader.LoadFile(context.Background(), path)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, fmt.Sprintf("%s(%s)", spec.Name, spec.Engine.Kind))
	}
	fmt.Printf("OK: %d agent(s): %v\n", len(specs), names)
	return nil
}

// validateGraph checks a graph document: the strict decode plus the wiring rules
// (one entry, unique node ids, edges that name real nodes). Node kinds are checked
// when a host builds the registry, which a standalone check cannot know.
func validateGraph(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	document, err := graph.Parse(body)
	if err != nil {
		return err
	}
	if err := nodes.Validate(context.Background(), path); err != nil {
		return err
	}
	fmt.Printf("OK: graph %q: %d node(s), %d edge(s), entry %q\n",
		document.Name, len(document.Nodes), len(document.Edges), document.Entry)
	return nil
}

// class names the layer that rejected the document, so the reader knows whether
// to look at the YAML or at the schema.
func class(err error) string {
	switch {
	case errdefs.IsValidation(err):
		return "parse"
	case errdefs.IsNotFound(err):
		return "missing"
	default:
		return "graph"
	}
}
