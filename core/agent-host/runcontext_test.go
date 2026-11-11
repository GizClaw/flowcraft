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
	d := &deployment{
		name:     "writer",
		tenant:   "acme",
		manifest: agent.Manifest{Deployment: "sha256:x"},
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
	if run.Manifest != d.manifest.ID() || run.Manifest == "" {
		t.Fatalf("manifest = %q, want %q", run.Manifest, d.manifest.ID())
	}
}
