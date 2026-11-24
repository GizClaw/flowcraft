package host

import (
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
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
