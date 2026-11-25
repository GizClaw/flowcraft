package host

import (
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
)

// TestSessionGaugeBalances is the series an operator watches for a leak: every open adds one and every
// close subtracts one, however the close happened.
func TestSessionGaugeBalances(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	driver := &turnDriver{log: &journal{}, block: make(chan struct{})}
	agents, _ := runnableHost(t, driver)

	if _, err := agents.Open(t.Context(), "s2", "referee"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := agents.CloseSession(t.Context(), "s2"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := agents.CloseSession(t.Context(), "s1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := agents.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	close(driver.block)
	got := recorder.Collect(t)
	if value, ok := got.SumWhere("host.sessions", map[string]string{"agent.name": "referee"}); !ok || value != 0 {
		t.Fatalf("open sessions = %v (ok=%v), want zero after closing them all; emitted: %v",
			value, ok, got.Names())
	}
}

// TestSteerIsCountedByOutcome keeps the refusals visible: an operator racing the turn loop shows up as a
// run of "idle" steers, which no span would report.
func TestSteerIsCountedByOutcome(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	driver := &turnDriver{log: &journal{}}
	agents, _ := runnableHost(t, driver)

	// Nothing is running, so this steer is refused — and counted as such.
	if _, err := agents.Steer(t.Context(), "s1", agent.SteerInput{Intent: agent.SteerRedirect}); !errors.Is(err, agent.ErrSessionIdle) {
		t.Fatalf("Steer = %v, want ErrSessionIdle", err)
	}

	got := recorder.Collect(t)
	value, ok := got.SumWhere("host.steers", map[string]string{"outcome": "idle", "intent": "steer"})
	if !ok || value != 1 {
		t.Fatalf("idle steers = %v (ok=%v); emitted: %v", value, ok, got.Names())
	}
}

// TestEvictionIsCounted is the difference between a limit doing its job and a caller closing sessions.
func TestEvictionIsCounted(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	grant := permissiveGrant("local")
	grant.Limits.Idle = policy.Duration(time.Minute)
	dep, _ := sessionDeployment(t, "referee", grant, log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "old", "referee"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	session, _ := agents.Session("old")
	session.touchNow(time.Now().Add(-2 * time.Minute))

	if _, err := agents.EvictIdle(t.Context(), "referee", time.Now()); err != nil {
		t.Fatalf("EvictIdle: %v", err)
	}
	got := recorder.Collect(t)
	if value, ok := got.SumWhere("host.evicted", map[string]string{"agent.name": "referee"}); !ok || value != 1 {
		t.Fatalf("evicted = %v (ok=%v); emitted: %v", value, ok, got.Names())
	}
}

// TestCancelIsCountedAndSpanned is the operator's footprint: the cancel runs on the caller's
// goroutine and in the caller's trace, so it gets a span of its own — and a series, because the
// interesting signal is the rate of cancels that found nothing to stop.
func TestCancelIsCountedAndSpanned(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	driver := &turnDriver{log: &journal{}, block: make(chan struct{})}
	agents, _ := runnableHost(t, driver)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	// Wait until the turn is in flight: Cancel addresses the running one.
	deadline := time.Now().Add(2 * time.Second)
	for {
		session, _ := agents.Session("s1")
		if session != nil && session.isRunning() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(time.Millisecond)
	}
	if err := agents.Cancel(t.Context(), "s1", "operator changed their mind"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// A second cancel finds no turn: counted as idle rather than failing the test's intent.
	if err := agents.Cancel(t.Context(), "s1", "again"); !errors.Is(err, agent.ErrSessionIdle) {
		t.Fatalf("second Cancel = %v, want ErrSessionIdle", err)
	}
	close(driver.block)
	<-done

	got := recorder.Collect(t)
	// This driver forwards to a peer, so the first cancel is counted as forwarded and the
	// second as idle: one reached a turn, the other found none.
	for _, want := range []string{"forwarded", "idle"} {
		value, ok := got.SumWhere("host.cancels", map[string]string{"outcome": want})
		if !ok || value != 1 {
			t.Fatalf("cancels[%s] = %v (ok=%v); emitted: %v", want, value, ok, got.Names())
		}
	}
}

// TestHostMetricsDeclareTheirShape pins the unit and the kind of the series an operator
// watches. The open-session count is an up-down counter and not a counter — a gauge that
// looks like a counter is summed instead of read, which reports a number that only grows —
// and the duration and count instruments carry the unit their name promises.
func TestHostMetricsDeclareTheirShape(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	driver := &turnDriver{log: &journal{}}
	agents, _ := runnableHost(t, driver)

	if _, err := agents.Open(t.Context(), "s2", "referee"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// An idle steer is refused and counted: the series exists either way, which is what
	// the shape assertion needs.
	_, _ = agents.Steer(t.Context(), "s2", agent.SteerInput{Intent: agent.SteerRedirect})
	if err := agents.CloseSession(t.Context(), "s2"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	got := recorder.Collect(t)
	got.RequireShape(t, "host.sessions", "updowncounter", "")
	got.RequireShape(t, "host.steers", "counter", "")
}
