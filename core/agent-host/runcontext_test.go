package host

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
)

// The deployment knows the tenant and the manifest; the run context is where
// both reach spans, tools and the RunRecord.
func TestRunContextCarriesTenantAndManifest(t *testing.T) {
	manifest := agent.Manifest{Deployment: "sha256:x"}
	// A built deployment caches the hash; a hand-built one has to say it, which is
	// what this test is: the run context stamps the cached value, not a hash
	// recomputed per run.
	d := &deployment{
		name:       "writer",
		tenant:     "acme",
		manifest:   manifest,
		manifestID: manifest.ID(),
	}
	run, ok := runctx.From(d.runContext(context.Background(), "conv-1"))
	if !ok {
		t.Fatal("run identity missing")
	}
	if run.SessionID != "conv-1" || run.AgentName != "writer" {
		t.Fatalf("run = %+v", run)
	}
	if run.TenantID != "acme" {
		t.Fatalf("tenant = %q, want acme", run.TenantID)
	}
	if run.Manifest != d.manifestID || run.Manifest == "" {
		t.Fatalf("manifest = %q, want %q", run.Manifest, d.manifestID)
	}
}
