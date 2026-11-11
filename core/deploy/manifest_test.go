package deploy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/message"
)

// A deployment records its version combination in the run's checkpoint, so a
// trace can be attributed without joining configuration stores.
func TestDeploymentRecordsAManifest(t *testing.T) {
	store := agenttest.NewMemStore()
	kinds := BuiltinKinds().WithEngines(GraphEngine(nodes.WithKinds(fixedKind{text: "hi"})))
	host := testHost(t)
	host.Store = store
	host.Harness = "test-harness"
	manager := newManager(t, kinds, host)
	spec := testSpec(t, "g", "graph")
	spec.Engine.Settings = rawSettings(map[string]any{
		"path": writeGraphDocument(t, fixedGraph),
	})
	if err := manager.Add(context.Background(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, ok := manager.Session("g", "conv-1")
	if !ok {
		t.Fatal("no session for the deployed graph")
	}
	for _, err := range session.Run(context.Background(),
		message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	checkpoint, err := store.LatestForSession(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("LatestForSession: %v", err)
	}
	raw, ok := checkpoint.Sections["manifest"]
	if !ok {
		t.Fatal("the checkpoint has no manifest")
	}
	var manifest agent.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Engine != "graph" || manifest.Deployment == "" {
		t.Errorf("manifest = %+v", manifest)
	}
	if manifest.Tools == "" || manifest.Models == "" {
		t.Errorf("manifest = %+v, want the catalog hashes", manifest)
	}
	if manifest.Harness != "test-harness" {
		t.Errorf("harness = %q, want the host's", manifest.Harness)
	}
	if manifest.ID() == "" {
		t.Error("the manifest has no id")
	}
}
