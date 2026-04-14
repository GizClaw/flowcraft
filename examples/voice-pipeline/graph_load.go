package main

import (
	"fmt"
	"os"

	"github.com/GizClaw/flowcraft/sdk/graph"

	"gopkg.in/yaml.v3"
)

// loadReactAgent unmarshals react_agent.yaml into a GraphDefinition.
const reactAgentYAML = "react_agent.yaml"

func loadReactAgent() (*graph.GraphDefinition, error) {
	data, err := os.ReadFile(reactAgentYAML)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", reactAgentYAML, err)
	}
	var def graph.GraphDefinition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return &def, nil
}
