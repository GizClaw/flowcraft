package a2a_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/agenttest"
	"github.com/GizClaw/flowcraft/sdkx/agent/a2a"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

// TestEngineSuite runs the shared agent.Engine conformance suite. The fake
// server answers message/send with an already-completed task, which is
// enough for every contract subtest: resume probes reject the suite's
// payload-less checkpoints with Validation, empty boards complete as a
// no-op, and interrupts observed before an instant completion legitimately
// skip.
func TestEngineSuite(t *testing.T) {
	f := newFakeA2A(t)
	f.sendFn = func(params json.RawMessage) (any, *rpcErr) {
		return map[string]any{"task": taskV1("t1", "c1", "TASK_STATE_COMPLETED", nil, nil)}, nil
	}

	agenttest.EngineSuite(t, func() (agent.Engine, agenttest.Capabilities) {
		eng, err := a2a.New(context.Background(),
			card(f.url(), false, a2aprotocol.Version),
			a2a.WithHTTPClient(&http.Client{}))
		if err != nil {
			t.Fatalf("a2a.New: %v", err)
		}
		return eng, agenttest.Capabilities{SupportsResume: true}
	})
}
