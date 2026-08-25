package delegation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/session"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestStreamEscrowStoreTakeRelease(t *testing.T) {
	service, err := NewService(boundDirectory(t, completedEngine("unused")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	sink := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	specs := []session.SinkSpec{{ID: "caller", Sink: sink}}
	release := service.storeStreamEscrow("ref-1", specs)

	got, ok := service.takeStreamEscrow("ref-1")
	if !ok || len(got) != 1 || got[0].ID != "caller" {
		t.Fatalf("takeStreamEscrow = %v, %v; want caller spec", got, ok)
	}
	// take returns a defensive copy.
	got[0].ID = "mutated"
	if again, _ := service.takeStreamEscrow("ref-1"); again[0].ID != "caller" {
		t.Fatalf("escrow was mutated by caller: %+v", again)
	}
	release()
	if _, ok := service.takeStreamEscrow("ref-1"); ok {
		t.Fatal("escrow survived release")
	}
	service.releaseStreamEscrow("ref-1") // idempotent
	service.releaseStreamEscrow("")      // no-op
}

func TestStreamEscrowExpiresByTTL(t *testing.T) {
	service, err := NewService(boundDirectory(t, completedEngine("unused")), nil,
		WithStreamEscrowTTL(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	sink := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	service.storeStreamEscrow("ref-ttl", []session.SinkSpec{{ID: "caller", Sink: sink}})
	time.Sleep(40 * time.Millisecond)
	if _, ok := service.takeStreamEscrow("ref-ttl"); ok {
		t.Fatal("expired escrow entry still resolvable")
	}
}

type captureAsyncBackend struct {
	submitted chan AsyncRequest
	submitErr error
}

func (b *captureAsyncBackend) Submit(_ context.Context, req AsyncRequest) (string, error) {
	if b.submitErr != nil {
		return "", b.submitErr
	}
	b.submitted <- req
	return "async-1", nil
}

func (b *captureAsyncBackend) Status(context.Context, string) (Response, error) {
	return Response{Status: StatusRunning}, nil
}

func TestDelegateAsyncStampsLineageStreamRefAndEscrow(t *testing.T) {
	backend := &captureAsyncBackend{submitted: make(chan AsyncRequest, 1)}
	service, err := NewService(boundDirectory(t, completedEngine("unused")), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	sink := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:              "caller",
			Sink:            sink,
			QueueSize:       7,
			DeliveryTimeout: time.Minute,
			Visibility:      session.VisibilityConfirmed,
			Authority:       session.AuthorityAuthoritative,
			AckMode:         session.AckExplicit,
			MaxUnacked:      3,
		}},
		Inheritable: true,
	})
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{
		Identity: agent.Identity{AgentID: "caller-agent", RunID: "run-caller"},
	})
	ctx = tool.WithCallID(ctx, "call-delegate-1")
	request := syncRequest("writer")
	request.Mode = ModeAsync
	response, err := service.Delegate(ctx, request)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if response.Status != StatusAccepted {
		t.Fatalf("response status = %q, want accepted", response.Status)
	}

	submitted := <-backend.submitted
	if submitted.ParentRunID != "run-caller" || submitted.CallID != "call-delegate-1" {
		t.Fatalf("submitted lineage = parent %q call %q", submitted.ParentRunID, submitted.CallID)
	}
	if submitted.Request.Metadata[ParentRunMetadataKey] != "run-caller" ||
		submitted.Request.Metadata[CallIDMetadataKey] != "call-delegate-1" {
		t.Fatalf("submitted metadata lineage = %+v", submitted.Request.Metadata)
	}
	if submitted.Stream == nil || submitted.Stream.Ref == "" {
		t.Fatalf("submitted stream ref = %+v, want escrow ref", submitted.Stream)
	}
	if submitted.Stream.Target != nil {
		t.Fatalf("submitted stream target = %+v, want nil without exporter", submitted.Stream.Target)
	}
	if submitted.Stream.Policy.QueueSize != 7 ||
		submitted.Stream.Policy.DeliveryTimeout != time.Minute.Milliseconds() ||
		submitted.Stream.Policy.Visibility != session.VisibilityConfirmed {
		t.Fatalf("stream policy snapshot = %+v", submitted.Stream.Policy)
	}
	service.streamEscrowMu.Lock()
	_, escrowed := service.streamEscrow[submitted.Stream.Ref]
	service.streamEscrowMu.Unlock()
	if !escrowed {
		t.Fatal("async submit did not leave an escrow entry")
	}
}

func TestDelegateAsyncExporterPopulatesTarget(t *testing.T) {
	backend := &captureAsyncBackend{submitted: make(chan AsyncRequest, 1)}
	exporter := func(spec session.SinkSpec) (StreamTarget, bool) {
		if spec.ID == "caller" {
			return StreamTarget{Kind: "conversation", ID: "ctx-1"}, true
		}
		return StreamTarget{}, false
	}
	service, err := NewService(boundDirectory(t, completedEngine("unused")), backend,
		WithStreamTargetExporter(exporter))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	sink := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "caller",
			Sink: sink,
		}},
		Inheritable: true,
	})
	request := syncRequest("writer")
	request.Mode = ModeAsync
	if _, err := service.Delegate(ctx, request); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	submitted := <-backend.submitted
	if submitted.Stream == nil || submitted.Stream.Target == nil {
		t.Fatalf("submitted stream = %+v, want escrow ref + conversation target", submitted.Stream)
	}
	if submitted.Stream.Target.Kind != "conversation" || submitted.Stream.Target.ID != "ctx-1" {
		t.Fatalf("exported target = %+v", submitted.Stream.Target)
	}
}

func TestWithStreamTargetExporterRejectsNil(t *testing.T) {
	if _, err := NewService(
		boundDirectory(t, completedEngine("unused")), nil,
		WithStreamTargetExporter(nil),
	); !errdefs.IsValidation(err) {
		t.Fatalf("nil exporter error = %v, want validation", err)
	}
}

func TestDelegateAsyncSubmitFailureReleasesEscrow(t *testing.T) {
	backend := &captureAsyncBackend{submitErr: errors.New("backend down")}
	service, err := NewService(boundDirectory(t, completedEngine("unused")), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	sink := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks:       []session.SinkSpec{{ID: "caller", Sink: sink}},
		Inheritable: true,
	})
	request := syncRequest("writer")
	request.Mode = ModeAsync
	if _, err := service.Delegate(ctx, request); err == nil {
		t.Fatal("Delegate succeeded, want submit failure")
	}
	service.streamEscrowMu.Lock()
	n := len(service.streamEscrow)
	service.streamEscrowMu.Unlock()
	if n != 0 {
		t.Fatalf("failed submit left %d escrow entries", n)
	}
}

func TestDelegateAsyncWithoutStreamPolicyHasNoStreamRef(t *testing.T) {
	backend := &captureAsyncBackend{submitted: make(chan AsyncRequest, 1)}
	service, err := NewService(boundDirectory(t, completedEngine("unused")), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	request := syncRequest("writer")
	request.Mode = ModeAsync
	if _, err := service.Delegate(context.Background(), request); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	submitted := <-backend.submitted
	if submitted.Stream != nil {
		t.Fatalf("submitted stream = %+v, want nil without stream policy", submitted.Stream)
	}
}

// TestAsyncDelegationStreamsLineageToCallerSinks is the async analogue
// of the sync inheritance test: the worker restores the caller's live
// sinks from the escrow, and the subagent's stream envelopes carry the
// same lineage headers, in real time, on the caller's sink instance.
func TestAsyncDelegationStreamsLineageToCallerSinks(t *testing.T) {
	received := make(chan event.Envelope, 16)
	sink := agent.StreamSinkFunc(func(
		_ context.Context,
		env event.Envelope,
		_ agent.StreamDeltaPayload,
	) error {
		received <- env
		return nil
	})
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		ctx = agent.WithRunInfo(ctx, run.Info())
		if err := agent.EmitStreamPart(
			ctx, host, run.RunID, "writer.node.work",
			message.TextPart{Text: "async progress"}); err != nil {
			return nil, err
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	backend := newQueueBackend()
	result := buildResult(t, delegationWithRunEnd(engine))
	directory := NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(directory, backend,
		WithMaxConcurrency(1), WithDeferredWorkers())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	manager := newTestSessionManagerForResult(t, result)
	if err := service.BindSessionManager(manager); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx := session.WithStreamPolicy(context.Background(), session.StreamPolicy{
		Sinks: []session.SinkSpec{{
			ID:   "caller",
			Sink: sink,
		}},
		Inheritable: true,
	})
	ctx = agent.WithRunInfo(ctx, agent.RunInfo{
		Identity: agent.Identity{AgentID: "caller-agent", RunID: "run-caller"},
	})
	ctx = tool.WithCallID(ctx, "call-delegate-1")
	request := syncRequest("writer")
	request.Mode = ModeAsync
	accepted, err := service.Delegate(ctx, request)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if accepted.Status != StatusAccepted {
		t.Fatalf("response status = %q, want accepted", accepted.Status)
	}

	select {
	case completed := <-backend.completed:
		if completed.Status != StatusSucceeded {
			t.Fatalf("completed = %+v", completed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not complete async work")
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-received:
			if env.ParentRunID() != "run-caller" || env.ToolCallID() != "call-delegate-1" {
				t.Fatalf("lineage headers = %+v", env.Headers)
			}
			goto released
		case <-deadline:
			t.Fatal("lineage-stamped async stream envelope never received")
		}
	}
released:
	waitUntil(t, 5*time.Second, func() bool {
		service.streamEscrowMu.Lock()
		defer service.streamEscrowMu.Unlock()
		return len(service.streamEscrow) == 0
	})
}

// TestAsyncDelegationResolverMaterializesTarget exercises the
// cross-process fallback: with no in-process escrow entry, the worker
// resolves a serializable StreamTarget through the registered resolver
// and attaches the resulting sink to the subagent turn.
func TestAsyncDelegationResolverMaterializesTarget(t *testing.T) {
	received := make(chan event.Envelope, 8)
	resolver := func(_ context.Context, target StreamTarget) (agent.StreamSink, error) {
		if target.Kind != "test" || target.ID != "chan-1" {
			t.Fatalf("resolver target = %+v", target)
		}
		return agent.StreamSinkFunc(func(
			_ context.Context,
			env event.Envelope,
			_ agent.StreamDeltaPayload,
		) error {
			received <- env
			return nil
		}), nil
	}
	engine := agent.EngineFunc(func(
		ctx context.Context,
		run agent.Run,
		host agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		ctx = agent.WithRunInfo(ctx, run.Info())
		if err := agent.EmitStreamPart(
			ctx, host, run.RunID, "writer.node.work",
			message.TextPart{Text: "resolved"}); err != nil {
			return nil, err
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	result := buildResult(t, delegationWithRunEnd(engine))
	directory := NewDirectory()
	if err := directory.Bind(result); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(directory, nil, WithStreamTargetResolver(resolver))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	manager := newTestSessionManagerForResult(t, result)
	if err := service.BindSessionManager(manager); err != nil {
		t.Fatal(err)
	}

	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{
		Identity: agent.Identity{AgentID: "caller-agent", RunID: "run-caller"},
	})
	ctx = tool.WithCallID(ctx, "call-delegate-2")
	req := AsyncRequest{
		Request: syncRequest("writer"),
		Depth:   1,
		Stream: &StreamRef{
			Target: &StreamTarget{Kind: "test", ID: "chan-1"},
		},
	}
	req.Request.Mode = ModeAsync
	response, err := service.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if response.Status != StatusSucceeded {
		t.Fatalf("response status = %q, want succeeded", response.Status)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-received:
			if env.ParentRunID() != "run-caller" || env.ToolCallID() != "call-delegate-2" {
				t.Fatalf("lineage headers = %+v", env.Headers)
			}
			return
		case <-deadline:
			t.Fatal("resolved-sink stream envelope never received")
		}
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if cond() {
		return
	}
	t.Fatal("condition not met before deadline")
}
