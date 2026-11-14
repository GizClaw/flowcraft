package main

import (
	"os"

	"github.com/GizClaw/flowcraft/core/runtime"
	"go.yaml.in/yaml/v3"
)

// placeholderEnv fills every name that is not already set.
func placeholderEnv(names []string) error {
	for _, name := range names {
		if _, set := os.LookupEnv(name); set {
			continue
		}
		if err := os.Setenv(name, "placeholder"); err != nil {
			return err
		}
	}
	return nil
}

// placeholderLayers reads the layer files one deployment document names and fills
// the references inside them. The path is resolved the way the loader resolves it
// (against the process working directory), and a file that cannot be read is left
// for the loader to report.
func placeholderLayers(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document struct {
		Agents []struct {
			Runtime *struct {
				Layers []struct {
					Path string `yaml:"path"`
				} `yaml:"layers"`
			} `yaml:"runtime"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil // the loader reports a malformed document with its own wording
	}
	for _, agent := range document.Agents {
		if agent.Runtime == nil {
			continue
		}
		for _, layer := range agent.Runtime.Layers {
			if layer.Path == "" {
				continue
			}
			layerBody, err := os.ReadFile(layer.Path)
			if err != nil {
				continue
			}
			requirement, err := runtime.RequiredPlaceholdersDocument(layerBody, layer.Path)
			if err != nil {
				return err
			}
			if err := placeholderEnv(requirement.Required); err != nil {
				return err
			}
		}
	}
	return nil
}
