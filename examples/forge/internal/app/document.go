package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/config"
	"github.com/GizClaw/flowcraft/core/deploy"

	"sigs.k8s.io/yaml"
)

// A workspace is two documents, which is the shape the platform resolves: the process (what this
// machine offers) and the agents (who runs and under what authority).
const (
	processFile = "runtime.yaml"
	agentsFile  = "deploy.yaml"
)

// WorkspaceScheme is the placeholder scheme forge registers for the workspace being opened. A document
// writes `${workspace:root}` where an absolute path is required, which is how one workspace directory
// stays readable and portable without a second path convention beside the document.
const WorkspaceScheme = "workspace"

// workspaceRoot is the only argument the scheme accepts.
const workspaceRoot = "root"

// workspaceOptions binds `${workspace:root}` to dir. Every layer the loader reads expands it the same
// way, because the resolver travels with the load rather than with a document.
func workspaceOptions(dir string) []config.Option {
	return []config.Option{
		config.WithResolver(WorkspaceScheme, func(_ context.Context, args []string) (string, error) {
			if len(args) != 1 || args[0] != workspaceRoot {
				return "", fmt.Errorf("${%s:...}: only ${%s:%s} is defined",
					WorkspaceScheme, WorkspaceScheme, workspaceRoot)
			}
			return dir, nil
		}),
	}
}

// processLayer and agentsLayer are the two documents, as layers: one file each, priority zero, named by
// their path so a load error points at the file.
func processLayer(dir string) config.Layer {
	path := filepath.Join(dir, processFile)
	return config.Layer{Name: path, Source: config.File(path)}
}

func agentsLayer(dir string) config.Layer {
	path := filepath.Join(dir, agentsFile)
	return config.Layer{Name: path, Source: config.File(path)}
}

// requiredEnvironment reports the variables the two documents name and the environment does not define.
//
// Expansion fails at the first unset reference, so reporting all of them in one pass is what keeps a
// missing key from being a restart loop. The report names every reference with its scheme; a bare name
// is an environment variable, and "workspace:root" is the one forge resolves itself.
func requiredEnvironment(ctx context.Context, dir string) ([]string, error) {
	requirement, err := config.RequiredPlaceholders(ctx, processLayer(dir), agentsLayer(dir))
	if err != nil {
		return nil, err
	}
	missing := requirement.Missing(os.LookupEnv)
	env := make([]string, 0, len(missing))
	for _, name := range missing {
		if !strings.Contains(name, ":") {
			env = append(env, name)
		}
	}
	return env, nil
}

// processFacts reads what the process document declares about models and tools, for Inspect.
//
// It reads the file instead of the plan: Inspect reports structure, and a workspace whose key is unset
// is exactly the workspace an operator is inspecting. A document that cannot be read is skipped.
func processFacts(dir string) (models, tools []string) {
	body, err := os.ReadFile(filepath.Join(dir, processFile))
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Inference struct {
			Providers []struct {
				Name     string `json:"name"`
				Settings struct {
					Models []struct {
						ID struct {
							Provider string `json:"provider"`
							Name     string `json:"name"`
						} `json:"id"`
					} `json:"models"`
				} `json:"settings"`
			} `json:"providers"`
		} `json:"inference"`
		Tools struct {
			Sources []struct {
				Name string `json:"name"`
			} `json:"sources"`
		} `json:"tools"`
	}
	if yaml.Unmarshal(body, &doc) != nil {
		return nil, nil
	}
	for _, provider := range doc.Inference.Providers {
		for _, declared := range provider.Settings.Models {
			models = append(models, declared.ID.Provider+":"+declared.ID.Name)
		}
	}
	for _, source := range doc.Tools.Sources {
		tools = append(tools, source.Name)
	}
	return models, tools
}

// deployAgents decodes the agents document far enough to name its agents, without resolving anything:
// Inspect describes a workspace it may not be able to build.
func deployAgents(dir string) []string {
	body, err := os.ReadFile(filepath.Join(dir, agentsFile))
	if err != nil {
		return nil
	}
	var doc struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if yaml.Unmarshal(body, &doc) != nil {
		return nil
	}
	names := make([]string, 0, len(doc.Agents))
	for _, entry := range doc.Agents {
		names = append(names, entry.Name)
	}
	return names
}

// process resolves the process document into a plan: what this machine offers, before anything is
// built.
func process(ctx context.Context, impls *deploy.Impls, dir string, opts []config.Option) (deploy.ProcessPlan, error) {
	return deploy.NewLoader(impls).WithConfigOptions(opts...).Process(ctx, processLayer(dir))
}

// deployments resolves the agents document against a *built* process.
//
// The capabilities come from the resource, not from the plan: a provider's settings are opaque to the
// document layer, so "which models this process serves" is only knowable once the providers exist. That
// ordering is the reason the two documents are resolved in two steps.
func deployments(ctx context.Context, impls *deploy.Impls, dir string, opts []config.Option, caps deploy.Capabilities) ([]deploy.Deployment, error) {
	return deploy.NewLoader(impls).WithConfigOptions(opts...).Deployments(ctx, caps, agentsLayer(dir))
}

// processSources is the tool sources the process document declares, by name.
func processSources(dir string) []string {
	body, err := os.ReadFile(filepath.Join(dir, processFile))
	if err != nil {
		return nil
	}
	var doc struct {
		Tools struct {
			Sources []struct {
				Name string `json:"name"`
			} `json:"sources"`
		} `json:"tools"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil
	}
	names := make([]string, 0, len(doc.Tools.Sources))
	for _, source := range doc.Tools.Sources {
		names = append(names, source.Name)
	}
	return names
}

// agentFacts is what one agent's entry says, read from the document rather than from a built deployment:
// Inspect answers "who is in this workspace", and a workspace whose key is unset is exactly the one
// being inspected.
type agentFacts struct {
	Name      string
	Graph     string
	Delegates []string
	Hooks     []string
}

func agentsFacts(dir string) []agentFacts {
	body, err := os.ReadFile(filepath.Join(dir, agentsFile))
	if err != nil {
		return nil
	}
	var doc struct {
		Agents []struct {
			Name    string `json:"name"`
			Program struct {
				Settings struct {
					Path string `json:"path"`
				} `json:"settings"`
			} `json:"program"`
			Grant struct {
				Delegation *struct {
					Targets []string `json:"targets"`
				} `json:"delegation"`
			} `json:"grant"`
			Hooks []struct {
				Impl string `json:"impl"`
			} `json:"hooks"`
		} `json:"agents"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil
	}
	facts := make([]agentFacts, 0, len(doc.Agents))
	for _, agent := range doc.Agents {
		one := agentFacts{
			Name:  agent.Name,
			Graph: expandWorkspace(agent.Program.Settings.Path, dir),
		}
		if agent.Grant.Delegation != nil {
			one.Delegates = agent.Grant.Delegation.Targets
		}
		for _, hook := range agent.Hooks {
			one.Hooks = append(one.Hooks, hook.Impl)
		}
		facts = append(facts, one)
	}
	return facts
}

// expandWorkspace resolves the one reference an inspecting reader can resolve without building anything:
// the workspace root. A path that names it becomes absolute; anything else is left as written.
func expandWorkspace(path, dir string) string {
	if path == "" {
		return ""
	}
	return strings.ReplaceAll(path, "${workspace:root}", dir)
}
