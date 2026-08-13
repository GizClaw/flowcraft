package agenttest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/event"
)

// fakeResumer accepts everything. It pins down that the suite passes
// a minimal correct Resumer; if this ever fails the suite has drifted
// from the agent.Resumer contract.
type fakeResumer struct{}

func (fakeResumer) CanResume(agent.Checkpoint) error { return nil }

func TestResumerSuite_FakeResumer(t *testing.T) {
	agenttest.ResumerSuite(t, func() agent.Resumer { return fakeResumer{} })
}

// fakeSink discards every delta. It pins down that the suite passes
// a minimal correct StreamSink.
type fakeSink struct{}

func (fakeSink) OnDelta(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
	return nil
}

func TestStreamSinkSuite_FakeSink(t *testing.T) {
	agenttest.StreamSinkSuite(t, func() agent.StreamSink { return fakeSink{} })
}
