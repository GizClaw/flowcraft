package apitest_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/internal/api/apitest"
	"github.com/GizClaw/flowcraft/internal/eventlog"
)

func draft(p string) eventlog.EnvelopeDraft {
	return eventlog.EnvelopeDraft{
		Partition: p,
		Type:      "task.submitted",
		Version:   1,
		Category:  "business",
		Payload:   map[string]any{"task_id": "t-1"},
		Actor:     &eventlog.Actor{ID: "u-1", Kind: "user", RealmID: "r-1"},
	}
}

// TestByteConsistencyAcrossTransports is the D.4 acceptance test: WS, SSE,
// and HTTP-pull MUST emit byte-equal envelope payloads.
func TestByteConsistencyAcrossTransports(t *testing.T) {
	rig := apitest.NewRig(t)
	const partition = "runtime:rt-1"
	rig.Seed(draft(partition), draft(partition), draft(partition))

	pull := rig.PullPartition(partition, 0, 100)
	if len(pull) != 3 {
		t.Fatalf("pull: want 3 envs, got %d", len(pull))
	}

	sse := rig.SSEPartition(partition, 0, 3, 2*time.Second)
	if len(sse) != 3 {
		t.Fatalf("sse: want 3 envs, got %d", len(sse))
	}

	ws := rig.WSPartition(partition, 0, 3, 2*time.Second)
	if len(ws) != 3 {
		t.Fatalf("ws: want 3 envs, got %d", len(ws))
	}

	for i := 0; i < 3; i++ {
		if !bytes.Equal(pull[i], sse[i]) {
			t.Fatalf("pull vs sse byte mismatch at %d:\n pull=%s\n sse=%s", i, pull[i], sse[i])
		}
		if !bytes.Equal(pull[i], ws[i]) {
			t.Fatalf("pull vs ws byte mismatch at %d:\n pull=%s\n ws=%s", i, pull[i], ws[i])
		}
	}
}
