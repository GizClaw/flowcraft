package deploy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

// The two documents below are the ones the design docs print. Keeping them here, and loading
// them, is the cheapest way to stop the documentation from describing a schema the code no
// longer accepts: a field renamed in one place breaks this test.
//
// Settings decode into a map because the test is about the document's shape, not about any
// one implementation's settings — a real implementation declares its own struct and gets strict
// decoding from that.
// platformRoots rebases the unix roots the documents are printed with onto this platform's
// spelling: a Windows path needs a volume, and the document layers judge absoluteness with
// filepath.IsAbs. Nothing is created, and nothing else in the text changes.
func platformRoots(document string) string {
	for _, root := range []string{"/srv/", "/opt/", "/tmp/", "/bin/", "/usr/"} {
		document = strings.ReplaceAll(document, root, pathtest.Roots()+strings.TrimPrefix(root, "/"))
	}
	return document
}

const documentedProcess = `
version: 1

# A backend holds no directory and no policy: the write set and the network posture come from
# the session's grant, and the working directory is the model's per-call input. What a backend
# takes in its settings is mechanism — mounts, profile templates, endpoints — and those land
# with the implementations.
sandbox:
  - {name: local, impl: local}
  - {name: hardened, impl: bwrap}

inference:
  providers:
    - name: gpt
      impl: openai
      settings:
        base_url: "https://api.openai.com/v1"
        api: chat
        api_key_env: "OPENAI_API_KEY"
        models:
          - id: {provider: openai, name: "gpt-5"}
    - name: gpt-fallback
      impl: azure
      settings:
        base_url: "https://example.openai.azure.com/v1"
        api: chat
        api_key_env: "AZURE_OPENAI_KEY"
        models:
          - id: {provider: openai, name: "gpt-5"}
  route:
    attempts_per_target: 2
    max_targets: 3
    base_delay: 250ms
    max_delay: 4s

observability: {trace: call, bodies: false}

tools:
  sources:
    - name: workspace
      impl: files
      settings:
        read: ["/srv/assets"]
        write: ["/srv/work"]
    - name: shell
      impl: exec
      settings:
        shell: "/bin/bash"
    - name: reference
      impl: mcp
      settings:
        # A server is a command the source starts, or a URL it reaches: the connection belongs to the
        # deployment, which is why an MCP tool declares no session capability.
        servers:
          - name: filesystem
            transport: stdio
            command: "npx"
            args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv/assets"]
          - name: docs
            transport: http
            url: "https://docs.internal/mcp"
            # A credential belongs here as a reference the host resolves — "Bearer ${env:TOKEN}" or a
            # scheme the application registered — never as a literal. This document is loaded by a
            # test, so it carries a plain header and needs nothing exported.
            headers: {X-Client: flowcraft}
  middleware:
    - {name: recover, impl: recover}
    - {name: telemetry, impl: telemetry}
    - {name: timeout, impl: timeout, settings: {seconds: 300}}
    - {name: truncate, impl: truncate, settings: {max_chars: 20000, dir: "/tmp/tools"}}

skills:
  roots: ["/srv/skills", "/opt/company-skills"]
`

const documentedAgents = `
version: 1

agents:
  - name: referee
    program:
      impl: graph
      settings: {path: "/srv/graphs/referee.yaml"}
    limits: {idle: 30m, max_sessions: 64}
    hooks:
      - impl: seat-memory
        settings: {dir: ".forge/memory", keep: 12}

  - name: seat_1
    program:
      impl: graph
      settings: {path: "/srv/graphs/seat_1.yaml"}
    limits: {idle: 10m}
`

// TestTheDocumentedConfigLoads is the example above, run.
func TestTheDocumentedConfigLoads(t *testing.T) {
	impls := deploy.NewImpls().
		Provider(deploy.Impl("openai", func(map[string]any) (*inference.Provider, error) {
			return &inference.Provider{}, nil
		})).
		Provider(deploy.Impl("azure", func(map[string]any) (*inference.Provider, error) {
			return &inference.Provider{}, nil
		})).
		// Backends take no settings yet, and struct{} says so: a document that invents one is
		// refused by the strict decoder instead of being absorbed by a permissive map.
		Backend(deploy.BackendImpl("local", func(struct{}, sandbox.Confine) (sandbox.Runner, error) {
			return nil, nil
		})).
		Backend(deploy.BackendImpl("bwrap", func(struct{}, sandbox.Confine) (sandbox.Runner, error) {
			return nil, nil
		})).
		ToolSource(deploy.Impl("files", func(map[string]any) (tool.Source, error) {
			return &fakeSource{}, nil
		})).
		ToolSource(deploy.Impl("exec", func(map[string]any) (tool.Source, error) {
			return &fakeSource{}, nil
		})).
		ToolSource(deploy.Impl("mcp", func(map[string]any) (tool.Source, error) {
			return &fakeSource{}, nil
		})).
		Middleware(deploy.MiddlewareImpl("recover", func(map[string]any, tool.Catalog) (tool.Middleware, error) {
			return nil, nil
		})).
		Middleware(deploy.MiddlewareImpl("telemetry", func(map[string]any, tool.Catalog) (tool.Middleware, error) {
			return nil, nil
		})).
		Middleware(deploy.MiddlewareImpl("timeout", func(map[string]any, tool.Catalog) (tool.Middleware, error) {
			return nil, nil
		})).
		Middleware(deploy.MiddlewareImpl("truncate", func(map[string]any, tool.Catalog) (tool.Middleware, error) {
			return nil, nil
		})).
		Hook(deploy.Impl("seat-memory", func(map[string]any) (agent.Hooks, error) {
			return agent.Hooks{}, nil
		})).
		Program(deploy.Impl("graph", func(map[string]any) (agent.Program, error) {
			return nil, nil
		}))
	// The documents below are printed with unix paths because that is what they document.
	// What this test checks is the schema, not that /srv exists, so the roots are rebased
	// onto the platform's own spelling: a Windows path needs a volume, and policy.Files
	// judges absoluteness with filepath.IsAbs.
	process := platformRoots(documentedProcess)
	agents := platformRoots(documentedAgents)
	loader := deploy.NewLoader(impls)
	plan, err := loader.Process(context.Background(), sources.Bytes("runtime.yaml", []byte(process)))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(plan.Providers) != 2 || len(plan.Backends) != 2 || len(plan.Tools) != 3 || len(plan.Middleware) != 4 {
		t.Fatalf("plan = %d providers, %d backends, %d tools, %d middleware",
			len(plan.Providers), len(plan.Backends), len(plan.Tools), len(plan.Middleware))
	}
	// How much of a turn is recorded is the process's, not an agent's: everything it controls is
	// built once for the process.
	if plan.Observability.Trace != telemetry.TraceCall || plan.Observability.Bodies {
		t.Fatalf("observability = %+v, want the documented trace level", plan.Observability)
	}
	deployments, err := loader.Deployments(context.Background(), sources.Bytes("agents.yaml", []byte(agents)))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 2 {
		t.Fatalf("deployments = %d, want 2", len(deployments))
	}
	referee := deployments[0]
	if referee.Name != "referee" || referee.Program.Impl != "graph" {
		t.Fatalf("referee = %+v", referee)
	}
	if referee.Limits.MaxSessions != 64 {
		t.Fatalf("referee limits = %+v", referee.Limits)
	}
	if len(referee.Hooks) != 1 || referee.Hooks[0].Impl != "seat-memory" {
		t.Fatalf("referee hooks = %+v", referee.Hooks)
	}
	if deployments[1].Limits.MaxSessions != 0 {
		t.Fatalf("seat_1 limits = %+v, want the omitted max_sessions as unbounded", deployments[1].Limits)
	}
}
