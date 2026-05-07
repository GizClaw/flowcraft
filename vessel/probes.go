package vessel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel/spec"

	otellog "go.opentelemetry.io/otel/log"
)

// probeRunner drives the per-vessel probe loop. It runs in its own
// goroutine, woken by a ticker at [spec.Probes.Interval]. On
// each tick every Liveness probe runs serially with a per-Check
// timeout of [spec.Probes.Timeout]; consecutive failures up
// to FailureThreshold trip the Captain into PhaseFailed and the
// configured [spec.Restart] decides what happens next.
//
// We keep the loop deliberately simple (no parallel Check, no
// jitter, no readiness gating). Those add little for a single-
// process daemon and would be the first things to revisit when the
// vessel runtime grows multi-host scheduling.
type probeRunner struct {
	cap *Captain
	cfg spec.Probes

	mu      sync.Mutex
	streaks map[string]int

	stopOnce sync.Once
	done     chan struct{}
}

const (
	defaultProbeInterval         = 30 * time.Second
	defaultProbeTimeout          = 5 * time.Second
	defaultProbeFailureThreshold = 3
)

func newProbeRunner(c *Captain, cfg spec.Probes) *probeRunner {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultProbeInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultProbeTimeout
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = defaultProbeFailureThreshold
	}
	return &probeRunner{
		cap:     c,
		cfg:     cfg,
		streaks: make(map[string]int, len(cfg.Liveness)),
		done:    make(chan struct{}),
	}
}

// start kicks off the loop in its own goroutine. The Captain calls
// this at the end of Launch (when rootCtx is fresh); stop is called
// from the Captain's teardown path.
func (p *probeRunner) start() {
	if p == nil || len(p.cfg.Liveness) == 0 {
		return
	}
	// Capture the rootCtx at start time. The Captain swaps rootCtx
	// to nil during teardown / restart; the loop must operate on
	// the launch-time value or it would dereference nil between
	// transitionToFailed and the next Launch.
	rootCtx := p.cap.rootCtx
	go p.loop(rootCtx)
}

// stop signals the loop to exit. Safe to call multiple times.
func (p *probeRunner) stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.done) })
}

func (p *probeRunner) loop(rootCtx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-rootCtx.Done():
			return
		case <-t.C:
			p.tick(rootCtx)
		}
	}
}

// tick runs one probe round. Probes execute serially so a slow
// probe cannot starve the others; if the per-Check Timeout is set
// generously the FailureThreshold catches genuine hangs after a
// few consecutive cycles.
func (p *probeRunner) tick(rootCtx context.Context) {
	for _, probe := range p.cfg.Liveness {
		select {
		case <-p.done:
			return
		case <-rootCtx.Done():
			return
		default:
		}
		ctx, cancel := context.WithTimeout(rootCtx, p.cfg.Timeout)
		started := time.Now()
		res, err := safeProbeCheck(ctx, probe)
		res.Latency = time.Since(started)
		cancel()

		if err != nil {
			p.recordFailure(probe.Name(), "", err.Error(), res.Detail)
			continue
		}
		if !res.Healthy {
			p.recordFailure(probe.Name(), res.Reason, "", res.Detail)
			continue
		}
		// Healthy: reset the streak so a subsequent failure starts
		// counting from zero — only consecutive failures count.
		p.mu.Lock()
		p.streaks[probe.Name()] = 0
		p.mu.Unlock()
	}
}

// recordFailure increments the failure streak for probe and emits a
// SubjectProbeFailed envelope on the bus. When the streak hits the
// configured threshold it transitions the Captain to PhaseFailed —
// the Captain's restart loop (when configured) handles recovery.
func (p *probeRunner) recordFailure(probe, reason, errMsg string, detail map[string]any) {
	p.mu.Lock()
	p.streaks[probe]++
	streak := p.streaks[probe]
	p.mu.Unlock()

	payload := ProbeFailedPayload{
		VesselID: p.cap.vs.ID,
		Probe:    probe,
		Reason:   reason,
		Error:    errMsg,
		Streak:   streak,
		Detail:   detail,
	}
	// Publish on rootCtx (not baseCtx) so the envelope ages out the
	// instant Stop fires its rootCancel — without this the probe
	// loop could race against bus close and try to publish onto a
	// closed bus. Surface envelope-build / publish errors via
	// telemetry instead of dropping them: the previous _ = swallow
	// made probe→bus regressions completely invisible.
	pubCtx := p.cap.rootCtx
	if pubCtx == nil {
		pubCtx = p.cap.baseCtx
	}
	env, envErr := event.NewEnvelope(pubCtx, SubjectProbeFailed, payload)
	if envErr != nil {
		telemetry.Warn(pubCtx, "vessel: probe envelope build failed",
			otellog.String("vessel_id", p.cap.vs.ID),
			otellog.String("probe", probe),
			otellog.String("error", envErr.Error()))
	} else {
		env.Source = p.cap.vs.ID
		if pubErr := p.cap.bus.Publish(pubCtx, env); pubErr != nil {
			telemetry.Warn(pubCtx, "vessel: probe publish failed",
				otellog.String("vessel_id", p.cap.vs.ID),
				otellog.String("probe", probe),
				otellog.String("error", pubErr.Error()))
		}
	}

	telemetry.Warn(pubCtx, "vessel: probe failed",
		otellog.String("vessel_id", p.cap.vs.ID),
		otellog.String("probe", probe),
		otellog.String("reason", reason),
		otellog.String("error", errMsg),
		otellog.Int("streak", streak))

	if streak >= p.cfg.FailureThreshold {
		cause := reason
		if cause == "" {
			cause = errMsg
		}
		p.cap.transitionToFailed(fmt.Sprintf("probe %q failed %d times: %s", probe, streak, cause))
	}
}

// safeProbeCheck recovers from panics inside a custom Probe.Check
// so a misbehaving probe cannot tear the Captain down. The recover
// is converted into a synthetic error so the regular failure path
// (envelope + streak increment) still applies.
func safeProbeCheck(ctx context.Context, p spec.Probe) (res spec.ProbeResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = spec.ProbeResult{Healthy: false}
			err = fmt.Errorf("vessel: probe %q panicked: %v", p.Name(), r)
		}
	}()
	return p.Check(ctx)
}

// LLMReachableProbe is the v0.1.0 built-in probe. It calls
// LLMResolver.Resolve(model) and runs a tiny no-op Generate against
// the returned LLM, treating any non-nil error as unhealthy.
//
// The probe is deliberately cheap (a single 1-token completion) so
// it can run on a 30-second cadence without blowing through quota.
// Callers wanting a richer reachability check (multi-region, JSON-
// schema validation, …) should ship their own [spec.Probe]
// implementation; this one exists to prove the wiring works
// end-to-end and to give simple deployments a useful default.
type LLMReachableProbe struct {
	// Resolver looks up the LLM. Non-optional; New[Probe] returns
	// nil when this is missing so callers fail fast.
	Resolver llm.LLMResolver

	// Model is the model string passed to Resolver.Resolve. Empty
	// uses the resolver's default ("" → fallback model).
	Model string

	// Label overrides the default probe name. Useful when more
	// than one LLMReachableProbe is registered (e.g. probing two
	// providers); empty defaults to "llm-reachable".
	Label string
}

// Name satisfies [spec.Probe].
func (p LLMReachableProbe) Name() string {
	if p.Label != "" {
		return p.Label
	}
	return "llm-reachable"
}

// Check satisfies [spec.Probe]. A non-nil error from Resolve
// or Generate flips Healthy=false; the error message becomes the
// failure Reason on the resulting ProbeResult.
func (p LLMReachableProbe) Check(ctx context.Context) (spec.ProbeResult, error) {
	if p.Resolver == nil {
		return spec.ProbeResult{Healthy: false, Reason: "resolver is nil"}, nil
	}
	inst, err := p.Resolver.Resolve(ctx, p.Model)
	if err != nil {
		return spec.ProbeResult{Healthy: false, Reason: "resolve: " + err.Error()}, nil
	}
	// One ping: empty user message, max_tokens=1. We don't care
	// about the reply, only that the call returns without error.
	_, _, err = inst.Generate(ctx, []model.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "ping"}}},
	}, llm.WithMaxTokens(int64(1)))
	if err != nil {
		return spec.ProbeResult{Healthy: false, Reason: "generate: " + err.Error()}, nil
	}
	return spec.ProbeResult{Healthy: true}, nil
}

// TokenBudgetProbe asserts the vessel has not consumed more than
// Threshold (a fraction in 0..1) of its
// spec.Resources.MaxTokensPerHour budget. It is a soft early-warning
// signal — by the time spec.Resources actually rejects Submits the
// vessel is already saturated; flipping a probe at 0.8 lets a
// RestartOnFailure policy preempt that, recycling state before
// users see admission failures.
//
// The probe needs visibility into the running Captain's tokenBudget,
// which only exists post-New. We satisfy this with a tiny private
// interface (budgetReader) that Captain.New populates immediately
// after constructing the budget; until then Check returns Healthy
// (probe runs are gated on the captain being in PhaseRunning anyway,
// so the "not yet wired" window never reaches the probe runner).
type TokenBudgetProbe struct {
	// Threshold is the saturation fraction at which the probe
	// flips Healthy=false. Defaults to 0.8 when zero / negative
	// / >1. Operators that want strict "100% or fail" semantics
	// pass 1.0; values close to 0 cause flapping under noise.
	Threshold float64

	// Label overrides the default name "token-budget". Useful when
	// multiple TokenBudgetProbes (e.g. paired with different
	// thresholds) share one vessel.
	Label string

	mu     sync.Mutex
	reader budgetReader
}

// budgetReader is the captain-internal seam that surfaces the
// hourly window total to the probe. We deliberately do NOT expose
// the *tokenBudget pointer itself — that would let probes mutate
// the budget, blowing the encapsulation Captain.Submit relies on.
type budgetReader interface {
	hourlyTotal() int64
	hourlyLimit() int64
}

// budgetReaderFor returns a budgetReader anchored at b. Captain.New
// resolves the probe slice for any TokenBudgetProbe and injects this
// adapter via setBudget; the probe's Check routine then debits
// against the running totals.
func budgetReaderFor(b *tokenBudget) budgetReader {
	if b == nil {
		return nil
	}
	return &captainBudgetReader{b: b}
}

type captainBudgetReader struct{ b *tokenBudget }

func (r *captainBudgetReader) hourlyTotal() int64 {
	if r == nil || r.b == nil {
		return 0
	}
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	r.b.rotateLocked(r.b.now())
	return r.b.hourTotalLocked()
}

func (r *captainBudgetReader) hourlyLimit() int64 {
	if r == nil || r.b == nil {
		return 0
	}
	return r.b.perHour
}

// setBudget is called by Captain.New right after the budget is
// constructed. Defensive against multiple captains sharing one
// probe instance: every set replaces the previous reader so the
// probe always reports the most recent captain's state.
func (p *TokenBudgetProbe) setBudget(b budgetReader) {
	p.mu.Lock()
	p.reader = b
	p.mu.Unlock()
}

// Name satisfies spec.Probe.
func (p *TokenBudgetProbe) Name() string {
	if p.Label != "" {
		return p.Label
	}
	return "token-budget"
}

// Check satisfies spec.Probe. Healthy is computed from the wired
// budgetReader: when no reader is set yet, OR the captain has no
// hourly limit configured, the probe is treated as Healthy (we
// have nothing meaningful to assert). Once both are present,
// total / limit must stay strictly below Threshold.
func (p *TokenBudgetProbe) Check(_ context.Context) (spec.ProbeResult, error) {
	p.mu.Lock()
	r := p.reader
	p.mu.Unlock()
	if r == nil {
		return spec.ProbeResult{Healthy: true, Reason: "budget reader not wired"}, nil
	}
	limit := r.hourlyLimit()
	if limit <= 0 {
		return spec.ProbeResult{Healthy: true, Reason: "no hourly limit configured"}, nil
	}
	threshold := p.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.8
	}
	used := r.hourlyTotal()
	ratio := float64(used) / float64(limit)
	detail := map[string]any{
		"hourly_used":  used,
		"hourly_limit": limit,
		"ratio":        ratio,
		"threshold":    threshold,
	}
	if ratio >= threshold {
		return spec.ProbeResult{
			Healthy: false,
			Reason:  fmt.Sprintf("hourly token usage %d/%d (%.0f%%) at or above %.0f%% threshold", used, limit, ratio*100, threshold*100),
			Detail:  detail,
		}, nil
	}
	return spec.ProbeResult{Healthy: true, Detail: detail}, nil
}

// ToolReachableProbe asserts a named Tool is still present in the
// shared tool registry (and optionally responsive). It catches the
// "tool registry was clobbered / unregistered out from under us"
// class of failure that callers care about when their workflows
// hard-depend on a specific tool: e.g. the dispatcher's
// kanban_submit auto-tool, or a custom RAG tool registered at
// daemon boot.
//
// When the registered Tool implements ToolHealthChecker, the probe
// also calls HealthCheck and propagates its result. Tools that
// don't implement it pass on presence alone — that is still useful
// as a "did somebody unregister this?" canary.
type ToolReachableProbe struct {
	// Registry is the same *tool.Registry the daemon hands to the
	// EngineFactory. Required.
	Registry *tool.Registry

	// ToolName is the name the probe checks for. Required.
	ToolName string

	// Label overrides the default "tool-reachable/<name>". Useful
	// when multiple probes target different tools in one vessel.
	Label string
}

// ToolHealthChecker is an optional contract a Tool may implement
// to report runtime health. Probes call it with a short timeout
// (the probe runner already wraps Check in spec.Probes.Timeout).
// Tools that don't implement it are treated as healthy whenever
// the registry returns them.
type ToolHealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// Name satisfies spec.Probe.
func (p *ToolReachableProbe) Name() string {
	if p.Label != "" {
		return p.Label
	}
	return "tool-reachable/" + p.ToolName
}

// Check satisfies spec.Probe. Two failure modes are surfaced:
//   - registry returns errdefs.NotFound: probe Healthy=false with
//     the registry's own error string.
//   - tool implements ToolHealthChecker and HealthCheck returns
//     non-nil: probe Healthy=false with that error.
//
// Anything else (including ctx cancellation) is propagated as the
// probe's err; the runner converts it to a failure streak entry.
func (p *ToolReachableProbe) Check(ctx context.Context) (spec.ProbeResult, error) {
	if p.Registry == nil {
		return spec.ProbeResult{Healthy: false, Reason: "registry is nil"}, nil
	}
	if p.ToolName == "" {
		return spec.ProbeResult{Healthy: false, Reason: "tool name is empty"}, nil
	}
	t, ok := p.Registry.Get(p.ToolName)
	if !ok {
		return spec.ProbeResult{Healthy: false, Reason: "tool not registered: " + p.ToolName}, nil
	}
	if hc, ok := t.(ToolHealthChecker); ok {
		if err := hc.HealthCheck(ctx); err != nil {
			return spec.ProbeResult{Healthy: false, Reason: "healthcheck: " + err.Error()}, nil
		}
	}
	return spec.ProbeResult{Healthy: true}, nil
}
