package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

type testResolver struct {
	instances map[string]*deploy.Instance
	calls     atomic.Int64
}

func (r *testResolver) Instance(id string) (*deploy.Instance, bool) {
	r.calls.Add(1)
	instance, ok := r.instances[id]
	return instance, ok
}

type nilResolver struct{}

func (*nilResolver) Instance(string) (*deploy.Instance, bool) { return nil, false }

type nilHostFactory struct{}

func (*nilHostFactory) NewHost(context.Context, HostRequest) (agent.Host, error) {
	return agent.NoopHost{}, nil
}

func newTestManager(t *testing.T, timeout time.Duration) (*Manager, *testResolver) {
	t.Helper()
	resolver := &testResolver{instances: map[string]*deploy.Instance{
		"agent-a": {},
		"agent-b": {},
	}}
	router := agent.NewStreamRouter(event.NewMemoryBus())
	t.Cleanup(func() { _ = router.Close() })
	manager, err := NewManager(
		resolver,
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return agent.NoopHost{}, nil
		}),
		router,
		WithIdleTimeout(timeout),
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, resolver
}

func TestKeyValidate(t *testing.T) {
	tests := []struct {
		name string
		key  Key
	}{
		{name: "empty", key: Key{}},
		{name: "missing agent", key: Key{ContextID: "ctx"}},
		{name: "missing context", key: Key{AgentID: "agent"}},
		{name: "blank agent", key: Key{AgentID: " \t", ContextID: "ctx"}},
		{name: "blank context", key: Key{AgentID: "agent", ContextID: "\n"}},
		{name: "padded agent", key: Key{AgentID: " agent", ContextID: "ctx"}},
		{name: "padded context", key: Key{AgentID: "agent", ContextID: "ctx "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.key.Validate(); !errdefs.IsValidation(err) {
				t.Fatalf("Validate() error = %v, want validation", err)
			}
		})
	}
	if err := (Key{AgentID: "agent", ContextID: "ctx"}).Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestContractsValidateTypedNil(t *testing.T) {
	var sink agent.StreamSinkFunc
	if err := (SinkSpec{ID: "sink", Sink: sink, QueueSize: 1}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("typed-nil sink error = %v, want validation", err)
	}
	if err := (SinkSpec{ID: "sink", Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	}), QueueSize: 1}).Validate(); err != nil {
		t.Fatalf("valid SinkSpec error = %v", err)
	}
	if err := (SinkSpec{ID: "sink", Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	}), QueueSize: -1}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("negative queue error = %v, want validation", err)
	}
	if err := (SinkSpec{ID: "sink", Sink: agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	}), DeliveryTimeout: -time.Millisecond}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("negative delivery timeout error = %v, want validation", err)
	}
	validSink := agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	})
	for _, spec := range []SinkSpec{
		{ID: "raw-explicit", Sink: validSink, AckMode: AckExplicit},
		{ID: "confirmed-observer-explicit", Sink: validSink, Visibility: VisibilityConfirmed, AckMode: AckExplicit},
		{ID: "raw-max-unacked", Sink: validSink, MaxUnacked: 1},
		{ID: "confirmed-observer-max-unacked", Sink: validSink, Visibility: VisibilityConfirmed, MaxUnacked: 1},
	} {
		if err := spec.Validate(); !errdefs.IsValidation(err) {
			t.Fatalf("invalid acknowledgement spec %+v error = %v, want validation", spec, err)
		}
	}
	if err := (SinkSpec{
		ID: "confirmed-authority", Sink: validSink,
		Visibility: VisibilityConfirmed, Authority: AuthorityAuthoritative,
		AckMode: AckExplicit, MaxUnacked: 1,
	}).Validate(); err != nil {
		t.Fatalf("valid confirmed authority error = %v", err)
	}

	request := HostRequest{
		Key:        Key{AgentID: "agent", ContextID: "ctx"},
		RunID:      "run",
		Interrupts: make(chan agent.Interrupt),
		AskUser:    AskUserFunc(func(context.Context, agent.UserPrompt) (agent.UserReply, error) { return agent.UserReply{}, nil }),
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid HostRequest error = %v", err)
	}
	request.AskUser = nil
	if err := request.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("nil AskUser error = %v, want validation", err)
	}
	factoryCalled := false
	factory := HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
		factoryCalled = true
		return agent.NoopHost{}, nil
	})
	if _, err := factory.NewHost(context.Background(), HostRequest{}); err != nil {
		t.Fatalf("HostFactoryFunc.NewHost() error = %v", err)
	}
	if !factoryCalled {
		t.Fatal("HostFactoryFunc did not invoke its function")
	}

	wantStates := []TurnState{
		TurnStarting, TurnRunning, TurnInterrupting, TurnCompleted, TurnInterrupted,
		TurnCanceled, TurnFailed, TurnAborted,
	}
	wantValues := []string{"starting", "running", "interrupting", "completed", "interrupted", "canceled", "failed", "aborted"}
	for i := range wantStates {
		if string(wantStates[i]) != wantValues[i] {
			t.Fatalf("TurnState[%d] = %q, want %q", i, wantStates[i], wantValues[i])
		}
	}
}

func TestManagerRejectsInvalidDependencies(t *testing.T) {
	router := agent.NewStreamRouter(event.NewMemoryBus())
	defer router.Close()
	host := HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
		return agent.NoopHost{}, nil
	})
	resolver := &testResolver{}

	tests := []struct {
		name     string
		resolver InstanceResolver
		host     HostFactory
		router   *agent.StreamRouter
	}{
		{name: "nil resolver", host: host, router: router},
		{name: "typed nil resolver", resolver: (*nilResolver)(nil), host: host, router: router},
		{name: "nil host", resolver: resolver, router: router},
		{name: "typed nil host", resolver: resolver, host: (*nilHostFactory)(nil), router: router},
		{name: "typed nil host func", resolver: resolver, host: HostFactoryFunc(nil), router: router},
		{name: "nil router", resolver: resolver, host: host},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewManager(tt.resolver, tt.host, tt.router); !errdefs.IsValidation(err) {
				t.Fatalf("NewManager() error = %v, want validation", err)
			}
		})
	}
}

func TestManagerRejectsInvalidOpenInputs(t *testing.T) {
	manager, resolver := newTestManager(t, time.Minute)
	if _, err := manager.Open(context.Background(), Key{}); !errdefs.IsValidation(err) {
		t.Fatalf("Open(invalid key) error = %v, want validation", err)
	}
	if _, err := manager.Open(nil, Key{AgentID: "agent-a", ContextID: "ctx"}); !errdefs.IsValidation(err) {
		t.Fatalf("Open(nil context) error = %v, want validation", err)
	}
	if _, err := manager.Open(context.Background(), Key{AgentID: "missing", ContextID: "ctx"}); !errdefs.IsNotFound(err) {
		t.Fatalf("Open(missing agent) error = %v, want not found", err)
	}
	resolver.instances["nil-agent"] = nil
	if _, err := manager.Open(context.Background(), Key{AgentID: "nil-agent", ContextID: "ctx"}); !errdefs.IsInternal(err) {
		t.Fatalf("Open(nil instance) error = %v, want internal", err)
	}
}

func TestManagerConcurrentSameKeyCreatesOneSession(t *testing.T) {
	manager, resolver := newTestManager(t, time.Minute)
	key := Key{AgentID: "agent-a", ContextID: "ctx"}

	const count = 64
	leases := make([]*Lease, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := range count {
		go func(i int) {
			defer wg.Done()
			lease, err := manager.GetOrCreate(context.Background(), key)
			if err != nil {
				t.Errorf("GetOrCreate() error = %v", err)
				return
			}
			leases[i] = lease
		}(i)
	}
	wg.Wait()

	first := leases[0].Session()
	for i, lease := range leases {
		if lease == nil || lease.Session() != first {
			t.Fatalf("lease[%d] does not share Session", i)
		}
		_ = lease.Close()
	}
	if got := resolver.calls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want 1", got)
	}
}

func TestManagerSeparatesAgentsSharingContext(t *testing.T) {
	manager, _ := newTestManager(t, time.Minute)
	a, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.Open(context.Background(), Key{AgentID: "agent-b", ContextID: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	if a.Session() == b.Session() {
		t.Fatal("different AgentIDs unexpectedly share Session")
	}
}

func TestLeaseCloseIsIdempotentAndPartialReleaseDoesNotReclaim(t *testing.T) {
	manager, _ := newTestManager(t, 15*time.Millisecond)
	key := Key{AgentID: "agent-a", ContextID: "ctx"}
	first, _ := manager.Open(context.Background(), key)
	second, _ := manager.Open(context.Background(), key)
	session := first.Session()

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)

	third, err := manager.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	defer second.Close()
	if third.Session() != session {
		t.Fatal("partial release reclaimed Session")
	}
}

func TestManagerFinalLeaseTimeoutReclaimsIdleSession(t *testing.T) {
	manager, _ := newTestManager(t, 15*time.Millisecond)
	key := Key{AgentID: "agent-a", ContextID: "ctx"}
	first, _ := manager.Open(context.Background(), key)
	session := first.Session()
	_ = first.Close()

	eventually(t, time.Second, func() bool { return manager.sessionCount() == 0 })
	second, err := manager.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Session() == session {
		t.Fatal("reclaimed Session was reused")
	}
}

func TestManagerActivityPreventsReclaimUntilIdle(t *testing.T) {
	manager, _ := newTestManager(t, 15*time.Millisecond)
	key := Key{AgentID: "agent-a", ContextID: "ctx"}
	lease, _ := manager.Open(context.Background(), key)
	session := lease.Session()
	session.changeActivity(activityTurn, 1)
	_ = lease.Close()
	time.Sleep(40 * time.Millisecond)
	if manager.sessionCount() != 1 {
		t.Fatal("active Session was reclaimed")
	}
	session.changeActivity(activityTurn, -1)
	eventually(t, time.Second, func() bool { return manager.sessionCount() == 0 })
}

func TestManagerReopenInvalidatesStaleTimer(t *testing.T) {
	manager, _ := newTestManager(t, 50*time.Millisecond)
	key := Key{AgentID: "agent-a", ContextID: "ctx"}
	first, _ := manager.Open(context.Background(), key)
	session := first.Session()
	_ = first.Close()

	time.Sleep(30 * time.Millisecond)
	second, err := manager.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	time.Sleep(40 * time.Millisecond)
	if second.Session() != session || manager.sessionCount() != 1 {
		t.Fatal("stale timer reclaimed reactivated Session")
	}
}

func TestManagerCloseRejectsNewLeasesAndDoesNotCloseBorrowedRouter(t *testing.T) {
	resolver := &testResolver{instances: map[string]*deploy.Instance{"agent-a": {}}}
	bus := event.NewMemoryBus()
	router := agent.NewStreamRouter(bus)
	t.Cleanup(func() {
		_ = router.Close()
		_ = bus.Close()
	})
	manager, err := NewManager(
		resolver,
		HostFactoryFunc(func(context.Context, HostRequest) (agent.Host, error) {
			return agent.NoopHost{}, nil
		}),
		router,
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, _ := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"})
	session := lease.Session()

	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if !session.isClosing() {
		t.Fatal("Manager.Close did not close Session")
	}
	if _, err := manager.Open(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"}); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Open() error = %v, want ErrManagerClosed", err)
	}
	if _, err := manager.GetOrCreate(context.Background(), Key{AgentID: "agent-a", ContextID: "ctx"}); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("GetOrCreate() error = %v, want ErrManagerClosed", err)
	}
	detach, err := router.Attach("run", "sink", agent.StreamSinkFunc(func(context.Context, event.Envelope, agent.StreamDeltaPayload) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("borrowed router was closed: %v", err)
	}
	detach()
	_ = lease.Close()
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
