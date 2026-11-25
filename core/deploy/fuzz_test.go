package deploy_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
)

// FuzzProcessDocument drives the loader with a document nobody wrote: a deployment
// document is untrusted input the same way a message is, and it is the input with the
// most surface — layers, overlays, strict per-implementation decode, three groups of
// entries and a cross-check against what the process carries.
//
// config.FuzzLoad covers the layer merge and stops at the canonical bytes. This one
// starts there and runs the plan loop: a duplicated name, a kind the table does not
// have, a settings object the implementation rejects, an entry that names another
// entry — each of those is an error path nothing else exercises as a whole.
//
// The invariant is the one a loader owes: every input is a document or an error, never
// a panic, and a plan that resolved has the names a grant and an agent document select
// by, so an empty one is a bug rather than a legal value.
func FuzzProcessDocument(f *testing.F) {
	for _, seed := range []string{
		processDoc,
		"version: 1\n",
		"version: 2\n",
		"",
		"null\n",
		"[]\n",
		"version: 1\nsandbox:\n  - {name: local, impl: local}\n  - {name: local, impl: local}\n",
		"version: 1\nsandbox:\n  - {name: local, impl: nope}\n",
		"version: 1\ninference:\n  providers:\n    - {name: gpt, impl: openai, settings: {unknown_field: 1}}\n",
		"version: 1\nobservability: {trace: none}\nagents:\n  - name: \"\"\n",
		"version: 1\nsandbox: []\ninference: {}\ntools: {}\n",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, document []byte) {
		impls := deploy.NewImpls().
			Provider(deploy.Impl("openai", func(map[string]any) (*inference.Provider, error) {
				return &inference.Provider{}, nil
			})).
			Backend(deploy.BackendImpl("local", func(map[string]any, sandbox.Confine) (sandbox.Runner, error) {
				return nil, nil
			})).
			ToolSource(deploy.Impl("files", func(map[string]any) (tool.Source, error) {
				return &fakeSource{}, nil
			})).
			Program(deploy.Impl("graph", func(map[string]any) (agent.Program, error) {
				return nil, nil
			}))
		loader := deploy.NewLoader(impls)
		layer := sources.Bytes("deploy.yaml", document)

		plan, err := loader.Process(context.Background(), layer)
		if err != nil {
			return
		}
		for _, name := range plan.BackendNames() {
			if name == "" {
				t.Fatalf("a backend resolved without a name; a grant selects it by that name\n%s", document)
			}
		}
		for _, provider := range plan.Providers {
			if provider.Name == "" || provider.Impl == "" {
				t.Fatalf("a provider resolved without a name or an implementation: %+v", provider)
			}
		}

		// A plan that resolved is the input of the deployment pass; an agent document
		// is resolved against what the process carries. Whatever it decides, it decides
		// without panicking — and a deployment it accepted is a deployment the host can
		// build, which starts with a name.
		deployments, err := loader.Deployments(context.Background(), layer)
		if err != nil {
			return
		}
		for _, dep := range deployments {
			if dep.Name == "" {
				t.Fatalf("a deployment resolved without a name\n%s", document)
			}
		}
		// The fingerprint is what a resume depends on: it has to survive being asked
		// for, including for a set that resolved to nothing.
		_ = deploy.Fingerprint(deployments)
	})
}
