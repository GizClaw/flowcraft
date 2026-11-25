package host

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
)

// The two documents below are the ones a deployment writes. They are read here, not built
// by hand: the point of this test is that the path from a document's bytes to a finished
// turn is one path, and every seam on it agrees with the others.
//
// The implementations behind the names are fakes, and that is a constraint rather than a
// shortcut: core may not import sandbox/local or tool/builtin (TestCoreDoesNotImportSiblingModules),
// so a real backend is the sandbox module's test to write. What is real here is every seam
// the document crosses — the loader's strict decode, the group tables, the capability
// questions, the plan's closures, the host's build of a session, the store, and the
// fingerprint a resume depends on.
const endToEndProcess = `
version: 1
sandbox:
  - name: local
    impl: local
    settings: { root: "/srv" }
inference:
  providers:
    - name: fake
      impl: fake
      settings: { model: "fake-1" }
tools:
  sources:
    - name: workspace
      impl: files
      settings: { root: "/srv" }
observability: { trace: run, bodies: false }
`

const endToEndAgent = `
version: 1
agents:
  - name: referee
    program: { impl: stub }
    grant:
      sandbox: local
      models: [{ id: { provider: "fake", name: "fake-1" } }]
      limits: { idle: 30m, max_sessions: 64 }
`

// endToEndImpls is the implementation table this binary would carry. Names and kinds are
// the document's; the values behind them are the test's.
func endToEndImpls(driver *seenDriver) *deploy.Impls {
	return deploy.NewImpls().
		Provider(deploy.Impl("fake", func(map[string]any) (*inference.Provider, error) {
			// A target has to serve a seam, and this test never calls it: the model is
			// dialled by the turn, and the turn is what the driver answers.
			return provider{name: "fake", journal: &journal{},
				models: []model.ModelDescriptor{modelRef("fake", "fake-1")}}.build(), nil
		})).
		Backend(deploy.BackendImpl("local", func(map[string]any, sandbox.Confine) (sandbox.Runner, error) {
			return &boundary{log: &journal{}, filesystemBounds: true}, nil
		})).
		ToolSource(deploy.Impl("files", func(map[string]any) (tool.Source, error) {
			return tooltest.Source(), nil
		})).
		Program(deploy.Impl("stub", func(map[string]any) (agent.Program, error) {
			return &seenProgram{driver: driver}, nil
		}))
}

// endToEndDocument loads both documents the way a host does: one loader, one table, one
// plan, then the agent pass against what the plan says it carries.
func endToEndDocument(t *testing.T, driver *seenDriver) (deploy.ProcessPlan, []deploy.Deployment) {
	t.Helper()
	loader := deploy.NewLoader(endToEndImpls(driver))
	process := sources.Bytes("runtime.yaml", []byte(endToEndProcess))

	plan, err := loader.Process(t.Context(), process)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// What the process offers is what a deployment may select: the backends the plan
	// configured and the models the providers serve.
	caps := deploy.Capabilities{Backends: plan.BackendNames(), Models: []string{"fake/fake-1"}}
	deployments, err := loader.Deployments(t.Context(), caps, sources.Bytes("agent.yaml", []byte(endToEndAgent)))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	return plan, deployments
}

// TestADeploymentDocumentRunsATurnEndToEnd is the whole core path in one test: a document's
// bytes become a plan through the loader and the registered factories, the plan becomes a
// Resource, a Deployment becomes an agent, a session runs a turn, the terminal state reaches
// the store, and a second process opened on that store continues the conversation.
//
// Each of those seams has its own test, and each of them passes a value it built itself.
// What none of them can check is that the value one seam produces is the value the next one
// reads — a plan field the host stopped reading, a group the table lost, a setting renamed
// in the document and nowhere else. That is this test's job, and it is why it never
// constructs a plan or a deployment by hand.
func TestADeploymentDocumentRunsATurnEndToEnd(t *testing.T) {
	driver := &seenDriver{log: &journal{}}
	plan, deployments := endToEndDocument(t, driver)
	if plan.Observability.Trace != telemetry.TraceRun {
		t.Fatalf("trace level = %v, want the document's", plan.Observability.Trace)
	}
	if len(deployments) != 1 || deployments[0].Name != "referee" {
		t.Fatalf("deployments = %+v", deployments)
	}

	store := agenttest.NewMemStore()
	fingerprint := deploy.Fingerprint(deployments)

	// First process: one turn, from a document to a terminal state.
	first := openEndToEnd(t, plan, store, fingerprint, deployments)
	var seen []agent.EventKind
	for event, err := range first.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		seen = append(seen, event.Kind())
	}
	if len(seen) != 2 || seen[0] != agent.KindText || seen[1] != agent.KindStatus {
		t.Fatalf("events = %v", seen)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second process: a new Resource and a new Agents table, the same store. The task the
	// first turn wrote is what the second turn must not overwrite.
	second := openEndToEnd(t, plan, store, fingerprint, deployments)
	t.Cleanup(func() { _ = second.Close(t.Context()) })
	for range second.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "again")) {
	}

	tasks := map[string]bool{}
	for _, request := range driver.requests {
		tasks[request.Task.ID] = true
	}
	if len(tasks) != 2 {
		t.Fatalf("task ids = %v, want two distinct ones across the restart", tasks)
	}
}

// openEndToEnd is one boot, the way a host does it: the plan is built into a Resource, the
// deployments are installed into a table, and the conversation is opened.
func openEndToEnd(t *testing.T, plan deploy.ProcessPlan, store agent.Store, fingerprint agent.Fingerprint, deployments []deploy.Deployment) *Agents {
	t.Helper()
	res, err := Open(t.Context(), plan, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	agents := NewAgents(res, WithStore(store), WithFingerprint(fingerprint))
	t.Cleanup(func() { _ = res.Close() })
	if err := agents.Add(deployments...); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee"); err != nil {
		t.Fatalf("Open session: %v", err)
	}
	return agents
}

// TestTheSameDocumentResolvesToTheSameFingerprint ties the document to the durability
// identity a resume compares: two loads of the same bytes must agree, and a changed
// document must not. The deployment fingerprint test pins the digest itself; this pins that
// the value the host stamps comes from the documents rather than from the build.
func TestTheSameDocumentResolvesToTheSameFingerprint(t *testing.T) {
	driver := &seenDriver{log: &journal{}}
	_, first := endToEndDocument(t, driver)
	_, second := endToEndDocument(t, driver)

	if deploy.Fingerprint(first) != deploy.Fingerprint(second) {
		t.Fatal("the same documents resolved to two fingerprints, so a resume would never match")
	}

	// A different grant is a different deployment set: resuming a task under it would run
	// the turn with authority the checkpoint was not written under.
	widened := append([]deploy.Deployment(nil), first...)
	widened[0].Grant.Models = append(widened[0].Grant.Models, model.ModelRef{
		ID: model.ModelID{Provider: "fake", Name: "other"},
	})
	if deploy.Fingerprint(widened) == deploy.Fingerprint(first) {
		t.Fatal("a widened grant kept the fingerprint, so a resume would run under a different authority")
	}
}
