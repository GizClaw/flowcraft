package vessel

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestTokenBudgetProbe_FlipsAtThreshold drives the probe directly:
// once hourly_used / hourly_limit ≥ Threshold the probe must report
// Healthy=false, otherwise Healthy=true. Without a threshold check
// the probe would either flap on noise (threshold too low) or never
// fire (threshold > 1).
func TestTokenBudgetProbe_FlipsAtThreshold(t *testing.T) {
	t.Parallel()
	b := newTokenBudget(0, 100)
	p := &TokenBudgetProbe{Threshold: 0.8}
	p.setBudget(budgetReaderFor(b))

	// 50/100 → 0.5, below threshold → healthy.
	u := b.begin("r", func() {})
	if err := b.add(u, 50); err != nil {
		t.Fatal(err)
	}
	res, err := p.Check(context.Background())
	if err != nil || !res.Healthy {
		t.Fatalf("at 50%%: healthy=%v err=%v reason=%q", res.Healthy, err, res.Reason)
	}

	// 80/100 → 0.8, at threshold → unhealthy.
	if err := b.add(u, 30); err != nil {
		t.Fatal(err)
	}
	res, err = p.Check(context.Background())
	if err != nil || res.Healthy {
		t.Fatalf("at 80%%: healthy=%v err=%v reason=%q", res.Healthy, err, res.Reason)
	}
}

// TestTokenBudgetProbe_NoLimitIsHealthy: a vessel without an
// hourly cap (the v0.1.0 default) MUST report healthy regardless of
// usage — the probe is meaningless without a limit to compare
// against, and reporting unhealthy would mass-flap every default
// vessel into Failed.
func TestTokenBudgetProbe_NoLimitIsHealthy(t *testing.T) {
	t.Parallel()
	b := newTokenBudget(0, 0)
	if b != nil {
		t.Fatal("budget should be nil when both caps are zero")
	}
	p := &TokenBudgetProbe{Threshold: 0.5}
	p.setBudget(budgetReaderFor(b))
	res, err := p.Check(context.Background())
	if err != nil || !res.Healthy {
		t.Fatalf("got healthy=%v err=%v", res.Healthy, err)
	}
}

// healthyTool / unhealthyTool exercise the optional ToolHealthChecker
// contract.
type healthyTool struct{}

func (healthyTool) Definition() model.ToolDefinition                    { return model.ToolDefinition{Name: "h"} }
func (healthyTool) Execute(_ context.Context, _ string) (string, error) { return "", nil }
func (healthyTool) HealthCheck(_ context.Context) error                 { return nil }

type unhealthyTool struct{ err error }

func (u unhealthyTool) Definition() model.ToolDefinition                    { return model.ToolDefinition{Name: "u"} }
func (u unhealthyTool) Execute(_ context.Context, _ string) (string, error) { return "", nil }
func (u unhealthyTool) HealthCheck(_ context.Context) error                 { return u.err }

// TestToolReachableProbe_PresenceAndHealth exercises every branch:
//   - tool absent  → unhealthy
//   - tool present, no HealthChecker → healthy
//   - tool present, HealthChecker returns nil → healthy
//   - tool present, HealthChecker returns err → unhealthy
func TestToolReachableProbe_PresenceAndHealth(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	probeFor := func(name string) *ToolReachableProbe {
		return &ToolReachableProbe{Registry: reg, ToolName: name}
	}

	// Absent.
	res, err := probeFor("nope").Check(context.Background())
	if err != nil || res.Healthy {
		t.Fatalf("absent: healthy=%v err=%v", res.Healthy, err)
	}

	// Present, no HealthChecker — wrap a FuncTool so the
	// type-assertion fails.
	plain := tool.FuncTool(model.ToolDefinition{Name: "plain"}, func(_ context.Context, _ string) (string, error) { return "ok", nil })
	reg.Register(plain)
	res, err = probeFor("plain").Check(context.Background())
	if err != nil || !res.Healthy {
		t.Fatalf("plain: healthy=%v err=%v", res.Healthy, err)
	}

	// Present + healthy HealthChecker.
	reg.Register(healthyTool{})
	res, err = probeFor("h").Check(context.Background())
	if err != nil || !res.Healthy {
		t.Fatalf("healthy: healthy=%v err=%v", res.Healthy, err)
	}

	// Present + failing HealthChecker.
	reg.Register(unhealthyTool{err: errors.New("kaboom")})
	res, err = probeFor("u").Check(context.Background())
	if err != nil || res.Healthy {
		t.Fatalf("unhealthy: healthy=%v err=%v reason=%q", res.Healthy, err, res.Reason)
	}
}

// TestToolReachableProbe_MissingRegistry guards the operator
// foot-gun where the YAML wired the probe before the registry was
// constructed (resolver bug). The probe should fail closed instead
// of nil-panic.
func TestToolReachableProbe_MissingRegistry(t *testing.T) {
	t.Parallel()
	p := &ToolReachableProbe{ToolName: "x"}
	res, err := p.Check(context.Background())
	if err != nil || res.Healthy {
		t.Fatalf("nil registry should yield Healthy=false: %+v %v", res, err)
	}
}

// TestProbes_SpecProbeAssertion: both new probes must satisfy the
// spec.Probe interface so they are accepted by spec.Probes.Liveness.
// Compile-time check.
var (
	_ spec.Probe = (*TokenBudgetProbe)(nil)
	_ spec.Probe = (*ToolReachableProbe)(nil)
)
