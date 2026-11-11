package deploy

import (
	"context"
	"testing"
)

func TestLoadDelegationPolicy(t *testing.T) {
	document := `
version: 1
agents:
  - name: planner
    engine: {kind: local}
    runtime:
      layers:
        - {priority: 0, path: ` + writeLayer(t) + `}
    delegation:
      targets: [worker]
      modes: [sync]
      max_depth: 2
      max_concurrent: 1
      timeout: 5s
      max_input_bytes: 1024
`
	specs, err := testLoader(t).Load(context.Background(), []byte(document))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	policy := specs[0].Delegation
	if policy == nil {
		t.Fatal("delegation policy is nil")
	}
	if len(policy.Targets) != 1 || policy.Targets[0] != "worker" {
		t.Fatalf("targets = %v", policy.Targets)
	}
	if policy.MaxDepth != 2 || policy.MaxConcurrent != 1 {
		t.Fatalf("limits = %d/%d", policy.MaxDepth, policy.MaxConcurrent)
	}
	if policy.Timeout.String() != "5s" || policy.MaxInputBytes != 1024 {
		t.Fatalf("timeout/bytes = %s/%d", policy.Timeout, policy.MaxInputBytes)
	}
}

func TestLoadDelegationRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"mode":    `delegation: {modes: [sometimes]}`,
		"timeout": `delegation: {timeout: soon}`,
		"depth":   `delegation: {max_depth: -1}`,
	}
	for name, block := range cases {
		document := `
version: 1
agents:
  - name: planner
    engine: {kind: local}
    runtime:
      layers:
        - {priority: 0, path: ` + writeLayer(t) + `}
    ` + block + `
`
		if _, err := testLoader(t).Load(context.Background(), []byte(document)); err == nil {
			t.Errorf("%s: Load accepted %s", name, block)
		}
	}
}
