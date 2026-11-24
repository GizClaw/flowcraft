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

// TestRecordCallIsTheProxysLedger is the record a proxied turn otherwise leaves nothing of: this
// side runs no model call and no tool, so the only fact it can write is the round trip — who
// answered, how long it took and how it ended.
func TestRecordCallIsTheProxysLedger(t *testing.T) {
	store := agenttest.NewMemStore()
	driver := &driver{}
	driver.mu.Lock()
	driver.remote = remote{TaskID: "peer-task", ContextID: "peer-ctx", State: agent.StateCompleted}
	driver.mu.Unlock()

	req := agenttest.Request()
	req.Task = agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking}
	req.Saver = saverFor(store)
	driver.recordCall(context.Background(), req, time.Now().Add(-20*time.Millisecond), nil)

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
		store := agenttest.NewMemStore()
		req := agenttest.Request()
		req.Task = agent.Task{ID: "t1"}
		req.Saver = saverFor(store)
		(&driver{}).recordCall(context.Background(), req, time.Now(), tc.err)

		events, err := store.Events(context.Background(), "t1")
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

// saverFor is the narrow half of a store, which is what a driver is handed: appending facts and
// reading the projection, and nothing that rewrites history.
func saverFor(store agent.Store) agent.Saver { return storeSaver{store: store} }

type storeSaver struct{ store agent.Store }

func (s storeSaver) Append(ctx context.Context, taskID string, events []agent.EventRecord) (int64, int64, error) {
	return s.store.Save(ctx, agent.Checkpoint{Schema: agent.Schema, Task: agent.Task{ID: taskID}}, events)
}

func (s storeSaver) Load(ctx context.Context, taskID string) (agent.Checkpoint, error) {
	return s.store.Load(ctx, taskID)
}
