package deploy

import (
	"context"
	"testing"
)

// The documented proxy agent has no runtime; Add used to reject it, so the whole
// proxy half of the package was unreachable from a deployment document.
func TestDocumentProxyAgentCanBeDeployed(t *testing.T) {
	specs, err := Loader{Engines: []Engine{{Name: "a2a"}}}.Load(context.Background(), []byte(`
version: 1
agents:
  - name: translator
    engine: {kind: a2a, peer: http://127.0.0.1:1}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manager := New()
	t.Cleanup(func() { _ = manager.Close() })
	deployed, err := manager.Add(context.Background(), specs[0])
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if deployed.Name() != "translator" {
		t.Errorf("name = %q", deployed.Name())
	}
}
