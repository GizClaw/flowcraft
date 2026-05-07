package apispec

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const sampleAll = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-default
spec:
  control:
    socket: /var/run/vesseld.sock
  shutdown:
    drainTimeout: 30s
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: support-bot
spec:
  agents: [boss, researcher]
  resources:
    maxConcurrentRuns: 4
    turnTimeout: 30s
  kanban:
    maxPendingTasks: 8
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: boss
spec:
  dispatcher: true
  historyAccess: read_write
  engine:
    ref: graph-llm
    config:
      llmProfile: openai-default
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: LLMProfile
metadata:
  name: openai-default
spec:
  provider: openai
  config:
    defaultModel: gpt-5.5
  auth:
    apiKey:
      valueFrom:
        env: OPENAI_API_KEY
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Probe
metadata:
  name: llm-health
spec:
  ref: llm-reachable
  config:
    llmProfile: openai-default
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: ToolPack
metadata:
  name: recall-pack
spec:
  ref: recall-builtin
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: HistoryStore
metadata:
  name: pg-conv
spec:
  ref: buffer
  config:
    maxMessages: 100
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Secret
metadata:
  name: openai-creds
spec:
  type: opaque
  stringData:
    api-key: sk-test
`

func TestDecodeAll_AllKinds(t *testing.T) {
	t.Parallel()
	objs, err := DecodeAll(strings.NewReader(sampleAll), "<test>")
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	if len(objs) != 8 {
		t.Fatalf("decoded %d objects, want 8", len(objs))
	}
	for _, obj := range objs {
		if err := obj.Validate(); err != nil {
			t.Errorf("validate %s/%s: %v", obj.GetTypeMeta().Kind, obj.GetObjectMeta().Name, err)
		}
	}
}

func TestDecodeAll_RejectsUnknownAPIVersion(t *testing.T) {
	t.Parallel()
	in := `apiVersion: vessel.flowcraft.io/v9
kind: Daemon
metadata:
  name: x
spec: {}`
	_, err := DecodeAll(strings.NewReader(in), "<test>")
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestDecodeAll_RejectsUnknownKind(t *testing.T) {
	t.Parallel()
	in := `apiVersion: vessel.flowcraft.io/v1alpha1
kind: Spaceship
metadata:
  name: x
spec: {}`
	_, err := DecodeAll(strings.NewReader(in), "<test>")
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestDecodeAll_RejectsMissingAPIVersion(t *testing.T) {
	t.Parallel()
	in := `kind: Daemon
metadata:
  name: x
spec: {}`
	_, err := DecodeAll(strings.NewReader(in), "<test>")
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected Validation, got %v", err)
	}
}

func TestDecodeAll_SkipsEmptyDocs(t *testing.T) {
	t.Parallel()
	in := `---
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: d
spec:
  control:
    socket: /tmp/v.sock
---
`
	objs, err := DecodeAll(strings.NewReader(in), "<test>")
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1 (empty docs should be dropped)", len(objs))
	}
	if objs[0].GetTypeMeta().Kind != v1alpha1.KindDaemon {
		t.Fatalf("kind = %s, want Daemon", objs[0].GetTypeMeta().Kind)
	}
}
