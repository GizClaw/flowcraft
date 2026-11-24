package a2a

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/goccy/go-json"
)

// TestPersistWritesTheRoundTrip is the record a proxied turn otherwise leaves nothing of: this
// side runs no model call and no tool, so the only fact it can write is the round trip — who
// answered, how long it took and how it ended. It goes in the same save as the peer's identity,
// so a resume point and the accounting of the call that produced it cannot disagree.
func TestPersistWritesTheRoundTrip(t *testing.T) {
	store := agenttest.NewMemStore()
	driver := &driver{store: store}
	driver.mu.Lock()
	driver.task = agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking}
	driver.remote = remote{TaskID: "peer-task", ContextID: "peer-ctx", State: agent.StateCompleted}
	driver.mu.Unlock()

	driver.persist(context.Background(), agenttest.Request(), time.Now().Add(-20*time.Millisecond), nil)

	events, err := store.Events(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Kind != RecordCall {
		t.Fatalf("events = %+v, want one %q", events, RecordCall)
	}
	var record CallRecord
	if err := json.Unmarshal(events[0].Payload, &record); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if record.PeerTaskID != "peer-task" || record.ContextID != "peer-ctx" {
		t.Fatalf("record identity = %+v", record)
	}
	if record.State != agent.StateCompleted || record.ErrorKind != "" {
		t.Fatalf("record outcome = %+v", record)
	}
	if record.LatencyMs < 20 {
		t.Fatalf("latency = %dms, want the measured round trip", record.LatencyMs)
	}
	// The peer's identity rides the same save, which is what a restart resumes from.
	saved, err := store.Load(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(saved.Sections[section]) == 0 {
		t.Fatal("the save carried no peer identity")
	}
}

// TestRecordCallClassifiesAFailure keeps the record readable on the branch that matters: a failed
// proxied call must name why, because the peer's own trace is a different process.
func TestRecordCallClassifiesAFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, callCanceled},
		{context.DeadlineExceeded, callTimeout},
	} {
		events, err := callEvents(remote{TaskID: "peer-task"}, time.Now(), tc.err)
		if err != nil || len(events) != 1 {
			t.Fatalf("events = %+v, %v", events, err)
		}
		var record CallRecord
		if err := json.Unmarshal(events[0].Payload, &record); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		if record.ErrorKind != tc.want {
			t.Fatalf("kind = %q, want %q", record.ErrorKind, tc.want)
		}
	}
}

// TestRoundCountsThePeersTransitions is the projection's "how far did this get" for a task nobody
// on this side steps through.
func TestRoundCountsThePeersTransitions(t *testing.T) {
	driver := &driver{}
	driver.setState(agent.StateWorking)
	driver.setState(agent.StateWorking)
	driver.setState(agent.StateCompleted)
	if got := driver.Round(); got != 2 {
		t.Fatalf("round = %d, want one per state change", got)
	}
	driver.adopt("other-task", "")
	if got := driver.Round(); got != 0 {
		t.Fatalf("round = %d after the peer minted a new task, want 0", got)
	}
}

// TestSendSpanCarriesThePeerAndTheLevel pins the one span this side opens: the round trip, with the
// peer's identity, and only from the level at which per-call spans exist.
func TestSendSpanCarriesThePeerAndTheLevel(t *testing.T) {
	recorder := telemetrytest.New(t)
	ctx := telemetry.WithTrace(context.Background(), telemetry.TraceCall)
	ctx, span := telemetry.Start(ctx, telemetry.TracerWithSuffix("a2a"), spanSend, telemetry.TraceCall)
	_ = ctx
	finishSendSpan(span, agent.StateCompleted, nil)
	span.End()
	recorder.RequireCount(t, spanSend, 1)

	recorder.Reset()
	quiet := telemetry.WithTrace(context.Background(), telemetry.TraceRun)
	_, span = telemetry.Start(quiet, telemetry.TracerWithSuffix("a2a"), spanSend, telemetry.TraceCall)
	finishSendSpan(span, agent.StateCompleted, nil)
	span.End()
	recorder.RequireCount(t, spanSend, 0)
}
