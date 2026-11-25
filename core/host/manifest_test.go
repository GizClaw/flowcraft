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
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &seenProgram{driver: &seenDriver{}}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents
}

// manifestGrant is the authority the manifest tests open with: one granted model and the
// process's whole tool table.
func manifestGrant() policy.Grant {
	return policy.Grant{Models: []policy.ModelGrant{{ModelRef: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}}}}
}

// sessionManifest is the combination one authority runs under: the value a session stamps on its
// records.
func sessionManifest(t *testing.T, agents *Agents, name string, grant policy.Grant) agent.Manifest {
	t.Helper()
	entry := manifestEntry(t, agents, name)
	view, err := entry.viewFor(grant.Tools, agents.res)
	if err != nil {
		t.Fatalf("viewFor: %v", err)
	}
	return agents.manifestFor(entry, grant, view)
}

// TestTheManifestNamesTheDeployment is the value's whole job: one assembled combination per
// authority, with every component a producer can answer for.
func TestTheManifestNamesTheDeployment(t *testing.T) {
	agents := manifestHost(t,
		WithFingerprint("fp-1"), WithHarness("forge@1.2.3"))
	manifest := sessionManifest(t, agents, "referee", manifestGrant())

	if manifest.Schema != agent.ManifestSchema {
		t.Fatalf("schema = %q, want %q", manifest.Schema, agent.ManifestSchema)
	}
	if manifest.Deployment != "fp-1" || manifest.Driver != "graph" {
		t.Fatalf("manifest = %+v, want the deployment and driver", manifest)
	}
	if manifest.Harness != "forge@1.2.3" {
		t.Fatalf("harness = %q, want the host's", manifest.Harness)
	}
	if manifest.Models == "" {
		t.Fatal("the model catalog the grant allows left no digest")
	}
	if manifest.Tools == "" {
		t.Fatal("the tool schema the authority exposes left no digest")
	}
	if manifest.ID() == "" {
		t.Fatal("the assembled manifest has no identity")
	}
}

// TestTwoDeploymentsOfOneProfessionAreComparable is why the digest exists at all: the same
// configuration assembled twice must produce one identity, or nothing joins a dataset row to
// another run.
func TestTwoDeploymentsOfOneProfessionAreComparable(t *testing.T) {
	first := sessionManifest(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee", manifestGrant())
	second := sessionManifest(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee", manifestGrant())
	if first.ID() != second.ID() {
		t.Fatalf("identical deployments disagree: %q vs %q", first.ID(), second.ID())
	}
}

// TestTheManifestFollowsTheToolsAndTheBuild pins what a change has to move: a deployment that
// exposes fewer tools, or a different build of the same documents, is not the same version
// combination and must not claim to be.
func TestTheManifestFollowsTheToolsAndTheBuild(t *testing.T) {
	reference := sessionManifest(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee", manifestGrant())

	rebuilt := sessionManifest(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.4")), "referee", manifestGrant())
	if rebuilt.ID() == reference.ID() {
		t.Fatal("a different build hashed the same")
	}

	// The authority decides the exposure: a session that may call nothing seen is a different
	// version combination, which is the point of assembling this per session.
	hidden := sessionManifest(t, manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3")), "referee",
		policy.Grant{Tools: policy.ToolSet{Allow: []string{}}})
	if hidden.ID() == reference.ID() {
		t.Fatal("a narrower authority hashed the same as a wider one")
	}
	if manifestTools(nil) != "" {
		t.Fatal("a session with no tool projection hashed something")
	}
}

// TestTheManifestReachesTheRunRecord is the reason the value is on the request: the record a
// sink stores must name the same combination the deployment was installed with.
func TestTheManifestReachesTheRunRecord(t *testing.T) {
	var got []agent.RunRecord
	agents := manifestHost(t, WithFingerprint("fp-1"), WithHarness("forge@1.2.3"),
		WithRunSink(func(record agent.RunRecord) { got = append(got, record) }))
	grant := manifestGrant()
	want := sessionManifest(t, agents, "referee", grant)

	if _, err := agents.Open(t.Context(), "s1", "referee", grant); err != nil {
		t.Fatalf("Open: %v", err)
	}
	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	if len(got) != 1 {
		t.Fatalf("run records = %d, want 1", len(got))
	}
	if got[0].Manifest != want.ID() {
		t.Fatalf("record manifest = %q, want the session's %q", got[0].Manifest, want.ID())
	}
}

// TestTheRequestCarriesThePrecomputedManifestID is the hot-path half of that record: the host
// hands the turn both the value and the identity it computed at open, so the turn never hashes a
// per-session constant.
func TestTheRequestCarriesThePrecomputedManifestID(t *testing.T) {
	agents, driver := seenHost(t)
	// seenHost opens its session with this authority, so that is the combination the record
	// must name.
	grant := permissiveGrant("local")
	want := sessionManifest(t, agents, "referee", grant)

	for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
	}

	req := driver.requests[0]
	if req.ManifestID == "" {
		t.Fatal("the request carries no manifest identity")
	}
	if req.ManifestID != want.ID() {
		t.Fatalf("request manifest id = %q, want the session's %q", req.ManifestID, want.ID())
	}
	if req.Manifest != want {
		t.Fatalf("request manifest = %+v, want the session's %+v", req.Manifest, want)
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
	base := manifestModels(declared("a"), []policy.ModelGrant{{ModelRef: ref}})
	if base == "" {
		t.Fatal("a granted and declared model has no digest")
	}
	if again := manifestModels(declared("a"), []policy.ModelGrant{{ModelRef: ref}}); again != base {
		t.Fatalf("the same catalog hashed differently: %q vs %q", again, base)
	}
	if changed := manifestModels(declared("b"), []policy.ModelGrant{{ModelRef: ref}}); changed == base {
		t.Fatal("a changed declaration did not change the digest")
	}
	if got := manifestModels(declared("a"), nil); got != "" {
		t.Fatalf("a deployment with no granted model hashed %q", got)
	}
	// A granted model the catalog does not declare still counts: a typo in a grant is a
	// different deployment, not an absent one.
	missing := model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-6"}}
	if got := manifestModels(declared("a"), []policy.ModelGrant{{ModelRef: missing}}); got == "" {
		t.Fatal("an undeclared granted model left no digest")
	}
}

// TestTheGrantDecidesTheModelDigest is the per-deployment half: two agents in one process with
// different model lists are different deployments, even though the catalog is shared.
func TestTheGrantDecidesTheModelDigest(t *testing.T) {
	ref := model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}
	provider := &inference.Provider{Catalog: catalog{models: []model.ModelDescriptor{{ID: ref.ID}}}}
	if manifestModels(provider, []policy.ModelGrant{{ModelRef: ref}}) == manifestModels(provider, nil) {
		t.Fatal("the granted model list did not reach the digest")
	}
	var _ = policy.Grant{}
}
