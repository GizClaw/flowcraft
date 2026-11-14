package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/runtime"

	"sigs.k8s.io/yaml"
)

// WorkspaceScheme is the placeholder scheme forge registers for the workspace
// being opened. A layer writes `${workspace:root}` where an absolute path is
// required, which is how one scenario directory stays readable and portable
// without a second path convention beside the document.
const WorkspaceScheme = "workspace"

// workspaceRoot is the only argument the scheme accepts.
const workspaceRoot = "root"

// documentFile is the deployment document at the root of every workspace.
const documentFile = "deploy.yaml"

// loadDocument reads and decodes a workspace's deployment document without
// expanding placeholders: forge needs the raw layer paths to report missing
// environment variables before anything is built.
func loadDocument(dir string) (deploy.Document, []byte, error) {
	path := filepath.Join(dir, documentFile)
	body, err := os.ReadFile(path)
	if err != nil {
		return deploy.Document{}, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc deploy.Document
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return deploy.Document{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Agents) == 0 {
		return deploy.Document{}, nil, fmt.Errorf("%s: no agents declared", path)
	}
	return doc, body, nil
}

// workspaceOptions binds the `${workspace:root}` scheme to dir. The loader
// carries these options, so the deployment document and every runtime layer it
// names expand the same reference the same way.
func workspaceOptions(dir string) []runtime.LoadOption {
	return []runtime.LoadOption{
		runtime.WithResolver(WorkspaceScheme, func(_ context.Context, args []string) (string, error) {
			if len(args) != 1 || args[0] != workspaceRoot {
				return "", fmt.Errorf(
					"${%s:...}: only ${%s:%s} is defined", WorkspaceScheme, WorkspaceScheme, workspaceRoot)
			}
			return dir, nil
		}),
	}
}

// documentLayers resolves the runtime layer stack each agent declares. A layer
// path is written relative to the document (`${workspace:root}/runtime.yaml`),
// so it is expanded here rather than trusted to mean something relative to the
// process working directory.
func documentLayers(doc deploy.Document, dir string) []runtime.Layer {
	var layers []runtime.Layer
	for _, agent := range doc.Agents {
		if agent.Runtime == nil {
			continue
		}
		for _, layer := range agent.Runtime.Layers {
			path := expandWorkspace(layer.Path, dir)
			layers = append(layers, runtime.Layer{
				Priority: layer.Priority,
				Name:     path,
				Path:     path,
			})
		}
	}
	return layers
}

// expandWorkspace substitutes the one reference forge resolves itself, for the
// paths it touches before the loader runs.
func expandWorkspace(path, dir string) string {
	return strings.ReplaceAll(path, "${"+WorkspaceScheme+":"+workspaceRoot+"}", dir)
}

// requiredEnvironment reports the variables the layer stack names and the
// environment does not define. Expansion fails at the first unset reference, so
// reporting all of them in one pass is what keeps a missing key from being a
// restart loop.
func requiredEnvironment(doc deploy.Document, dir string) ([]string, error) {
	requirement, err := runtime.RequiredPlaceholders(documentLayers(doc, dir)...)
	if err != nil {
		return nil, err
	}
	missing := requirement.Missing(os.LookupEnv)
	// The report names every reference, scheme and all. A bare name is an
	// environment variable; "workspace:root" is the scheme forge resolves
	// itself, so it can never be missing from the environment.
	env := make([]string, 0, len(missing))
	for _, name := range missing {
		if strings.Contains(name, ":") {
			continue
		}
		env = append(env, name)
	}
	return env, nil
}

// layerFacts reads what an agent's layer stack declares about models and tools.
//
// It reads the files instead of expanding the stack: Inspect reports structure, and
// a workspace whose key is unset is exactly the workspace an operator is inspecting.
// A layer that cannot be read is skipped - the report is a description, not a gate.
func layerFacts(dir string, doc *deploy.RuntimeDocument) (models, tools []string) {
	if doc == nil {
		return nil, nil
	}
	for _, layer := range doc.Layers {
		body, err := os.ReadFile(expandWorkspace(layer.Path, dir))
		if err != nil {
			continue
		}
		var layerDoc struct {
			Inference struct {
				Models []struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				} `json:"models"`
			} `json:"inference"`
			Tools struct {
				Enable []string `json:"enable"`
			} `json:"tools"`
		}
		if yaml.Unmarshal(body, &layerDoc) != nil {
			continue
		}
		for _, model := range layerDoc.Inference.Models {
			models = append(models, model.Provider+":"+model.Model)
		}
		tools = append(tools, layerDoc.Tools.Enable...)
	}
	return models, tools
}
