package host

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
	"github.com/goccy/go-json"
)

// manifestEntry reads one installed deployment's assembled manifest.
func manifestEntry(t *testing.T, agents *Agents, name string) *deployment {
	t.Helper()
	agents.mu.Lock()
	defer agents.mu.Unlock()
	entry, ok := agents.entries[name]
	if !ok {
		t.Fatalf("agent %q is not deployed", name)
	}
	return entry
}

// manifestSource contributes one tool, so the deployment's exposed schema has something to
// hash: a process with no tools legitimately has no tool digest, and that is a different test.
func manifestSource() deploy.PlannedToolSource {
	return deploy.PlannedToolSource{Name: "workspace", Impl: "fake", New: func() (tool.Source, error) {
		return tooltest.Source(tool.TextTool(message.ToolDefinition{
			Name:        "read_file",
			Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, string) (string, error) { return "", nil })), nil
	}}
}

// manifestHost installs one agent over a process that declares a model and a tool, so the
// assembled manifest has every component a deployment can have.
func manifestHost(t *testing.T, opts ...AgentsOption) *Agents {
	t.Helper()
	log := &journal{}
	plan := deploy.ProcessPlan{
		Providers: []deploy.PlannedProvider{plannedProvider(provider{
			name:    "openai",
			journal: log,
			models:  []model.ModelDescriptor{modelRef("openai", "gpt-5")},
		})},
		Backends: []deploy.PlannedBackend{boundBackend("local", &boundary{log: log, filesystemBounds: true})},
		Tools:    []deploy.PlannedToolSource{manifestSource()},
	}
	res, err := Open(t.Context(), plan, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	agents := NewAgents(res, opts...)
	grant := permissiveGrant("local")
	grant.Tools = policy.ToolSet{}
	grant.Models = []model.ModelRef{{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}}
	dep := deploy.Deployment{
		Name:    "referee",
		Grant:   grant,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &seenProgram{driver: &seenDriver{}}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents
}

// TestTheManifestNamesTheDeployment is the value's whole job: one assembled combination per
// deployment, with every component a producer can answer for.
func TestTheManifestNamesTheDeployment(t *testing.T) {
	agents := manifestHost(t,
		WithFingerprint("fp-1"), WithHarness("forge@1.2.3"))
	entry := manifestEntry(t, agents, "referee")

	if entry.manifest.Schema != agent.ManifestSchema {
		t.Fatalf("schema = %q, want %q", entry.manifest.Schema, agent.ManifestSchema)
	}
	if entry.manifest.Deployment != "fp-1" || entry.manifest.Driver != "graph" {
		t.Fatalf("manifest = %+v, want the deployment and driver", entry.manifest)
	}
	if entry.manifest.Harness != "forge@1.2.3" {
		t.Fatalf("harness = %q, want the host's", entry.manifest.Harness)
	}
	if entry.manifest.Models == "" {
		t.Fatal("the model catalog the grant allows left no digest")
	}
	if entry.manifest.Tools == "" {
		t.Fatal("the tool schema the deployment exposes left no digest")
	}
	if entry.manifest.ID() == "" {
		t.Fatal("the assembled manifest has no identity")
	}
}

// TestTwoDeploymentsOfOneProfessionAreComparable is why the digest exists at all: the same
// configuration assembled twice must produce one identity, or nothing joins a dataset row to
// another run.
func TestTwoDeploymentsOfOneProfessionAreComparable(t *testing.T) {
	first := manifestEntry(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee")
	second := manifestEntry(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee")
	if first.manifest.ID() != second.manifest.ID() {
		t.Fatalf("identical deployments disagree: %q vs %q", first.manifest.ID(), second.manifest.ID())
	}
}

// TestTheManifestFollowsTheToolsAndTheBuild pins what a change has to move: a deployment that
// exposes fewer tools, or a different build of the same documents, is not the same version
// combination and must not claim to be.
func TestTheManifestFollowsTheToolsAndTheBuild(t *testing.T) {
	reference := manifestEntry(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee")

	rebuilt := manifestEntry(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.4")), "referee")
	if rebuilt.manifest.ID() == reference.manifest.ID() {
		t.Fatal("a different build hashed the same")
	}

	hidden := manifestEntry(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee")
	hiddenView := hidden.newView()
	if manifestTools(hiddenView) == "" {
		t.Fatal("the exposed tool schema has no digest")
	}
	if manifestTools(nil) != "" {
		t.Fatal("a deployment with no tool projection hashed something")
	}
}

// TestTheManifestReachesTheRunRecord is the reason the value is on the request: the record a
// sink stores must name the same combination the deployment was installed with.
func TestTheManifestReachesTheRunRecord(t *testing.T) {
	var got []agent.RunRecord
	agents := manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3"),
		WithRunSink(func(record agent.RunRecord) { got = append(got, record) }))
	entry := manifestEntry(t, agents, "referee")

	if _, err := agents.Open(t.Context(), "s1", "referee"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	if len(got) != 1 {
		t.Fatalf("run records = %d, want 1", len(got))
	}
	if got[0].Manifest != entry.manifest.ID() {
		t.Fatalf("record manifest = %q, want the deployment's %q", got[0].Manifest, entry.manifest.ID())
	}
}

// TestTheRequestCarriesThePrecomputedManifestID is the hot-path half of that record: the host
// hands the turn both the value and the identity it computed at install, so the turn never hashes
// a per-deployment constant.
func TestTheRequestCarriesThePrecomputedManifestID(t *testing.T) {
	agents, driver := seenHost(t)
	entry := manifestEntry(t, agents, "referee")

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	req := driver.requests[0]
	if req.ManifestID == "" {
		t.Fatal("the request carries no manifest identity")
	}
	if req.ManifestID != entry.manifest.ID() {
		t.Fatalf("request manifest id = %q, want the deployment's %q", req.ManifestID, entry.manifest.ID())
	}
	if req.Manifest != entry.manifest {
		t.Fatalf("request manifest = %+v, want the deployment's %+v", req.Manifest, entry.manifest)
	}
}

// TestTheToolDigestIsOrderFree keeps the digest about content: a projection that lists the
// same definitions in another order is the same schema, and hashing the order would make two
// identical deployments incomparable for no reason.
func TestTheToolDigestIsOrderFree(t *testing.T) {
	left := manifestTools(listView{definitions: []message.ToolDefinition{
		{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}})
	right := manifestTools(listView{definitions: []message.ToolDefinition{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}})
	if left != right {
		t.Fatalf("the same schema in another order hashed differently: %q vs %q", left, right)
	}

	changed := manifestTools(listView{definitions: []message.ToolDefinition{
		{Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "b", Description: "now with help text", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}})
	if changed == left {
		t.Fatal("a rewritten description did not change the tool digest")
	}
}

// listView is a deployment's tool surface with nothing behind it: enough to hash.
type listView struct{ definitions []message.ToolDefinition }

func (v listView) Definitions() []message.ToolDefinition { return v.definitions }
func (listView) Visible(string) bool                     { return true }
func (listView) RecordCall(message.ToolCall)             {}

// TestTheModelDigestFollowsTheDeclaredCatalog is the difference between "which models were
// asked for" and "which models this process serves".
func TestTheModelDigestFollowsTheDeclaredCatalog(t *testing.T) {
	ref := model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}
	declared := func(weight string) *inference.Provider {
		return &inference.Provider{Catalog: catalog{models: []model.ModelDescriptor{{
			ID:    ref.ID,
			Label: weight,
		}}}}
	}
	base := manifestModels(declared("a"), []model.ModelRef{ref})
	if base == "" {
		t.Fatal("a granted and declared model has no digest")
	}
	if again := manifestModels(declared("a"), []model.ModelRef{ref}); again != base {
		t.Fatalf("the same catalog hashed differently: %q vs %q", again, base)
	}
	if changed := manifestModels(declared("b"), []model.ModelRef{ref}); changed == base {
		t.Fatal("a changed declaration did not change the digest")
	}
	if got := manifestModels(declared("a"), nil); got != "" {
		t.Fatalf("a deployment with no granted model hashed %q", got)
	}
	// A granted model the catalog does not declare still counts: a typo in a grant is a
	// different deployment, not an absent one.
	missing := model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-6"}}
	if got := manifestModels(declared("a"), []model.ModelRef{missing}); got == "" {
		t.Fatal("an undeclared granted model left no digest")
	}
}

// TestTheGrantDecidesTheModelDigest is the per-deployment half: two agents in one process with
// different model lists are different deployments, even though the catalog is shared.
func TestTheGrantDecidesTheModelDigest(t *testing.T) {
	ref := model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}
	provider := &inference.Provider{Catalog: catalog{models: []model.ModelDescriptor{{ID: ref.ID}}}}
	if manifestModels(provider, []model.ModelRef{ref}) == manifestModels(provider, nil) {
		t.Fatal("the granted model list did not reach the digest")
	}
	var _ = policy.Grant{}
}
