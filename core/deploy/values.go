package deploy

import (
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference/route"
)

// parseRoute turns the written retry policy into the value the router takes.
//
// Delays are strings because a document writes "250ms": the unit belongs to the reader, and a
// bare number would be nanoseconds to nobody's benefit.
func parseRoute(doc routeDocument) (route.RetryPolicy, error) {
	var retry route.RetryPolicy
	if doc.AttemptsPerTarget < 0 {
		return retry, errdefs.Validationf("deploy: route.attempts_per_target must not be negative")
	}
	if doc.MaxTargets < 0 {
		return retry, errdefs.Validationf("deploy: route.max_targets must not be negative")
	}
	base, err := parseDelay("route.base_delay", doc.BaseDelay)
	if err != nil {
		return retry, err
	}
	maxDelay, err := parseDelay("route.max_delay", doc.MaxDelay)
	if err != nil {
		return retry, err
	}
	if base > 0 && maxDelay > 0 && maxDelay < base {
		return retry, errdefs.Validationf(
			"deploy: route.max_delay %s is below base_delay %s", maxDelay, base)
	}
	retry.AttemptsPerTarget = doc.AttemptsPerTarget
	retry.MaxTargets = doc.MaxTargets
	retry.BaseDelay = base
	retry.MaxDelay = maxDelay
	return retry, nil
}

// observability reads the process's record level.
//
// Both fields are required. A document that leaves one out would inherit something, and there
// is nothing to inherit: how much traffic is recorded is a statement about this process, and
// silence does not make it. The process document used to leave it to each agent; it moved up
// because nothing below the assembly can honour a per-agent answer — the tool chain and the
// model access are built once, for the process.
func observability(doc observabilityDocument) (Observability, error) {
	if strings.TrimSpace(doc.Trace) == "" {
		return Observability{}, errdefs.Validationf(
			"deploy: observability.trace is required (none, run, call or node)")
	}
	trace, err := agent.ParseTraceLevel(doc.Trace)
	if err != nil {
		return Observability{}, errdefs.Validationf("deploy: observability.trace: %v", err)
	}
	if doc.Bodies == nil {
		return Observability{}, errdefs.Validationf(
			"deploy: observability.bodies is required (true or false)")
	}
	return Observability{Trace: trace, Bodies: *doc.Bodies}, nil
}

// parseDelay reads a duration string, naming the field when it does not parse.
func parseDelay(field, value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, errdefs.Validationf("deploy: %s: %v", field, err)
	}
	if parsed < 0 {
		return 0, errdefs.Validationf("deploy: %s must not be negative", field)
	}
	return parsed, nil
}
