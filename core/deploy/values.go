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

// observability reads one agent's record level.
//
// Both fields are required. A document that leaves one out would inherit something, and there
// is nothing to inherit: how much of an agent's traffic is recorded is a statement about that
// agent, and silence does not make it. The process document used to carry defaults for these;
// removing them removed the ambiguity with them.
func observability(doc observabilityDocument, agentName string) (agent.TraceLevel, bool, error) {
	if strings.TrimSpace(doc.Trace) == "" {
		return 0, false, errdefs.Validationf(
			"deploy: agent %q must state observability.trace (none, run, call or node)", agentName)
	}
	trace, err := agent.ParseTraceLevel(doc.Trace)
	if err != nil {
		return 0, false, errdefs.Validationf("deploy: agent %q: %v", agentName, err)
	}
	if doc.Bodies == nil {
		return 0, false, errdefs.Validationf(
			"deploy: agent %q must state observability.bodies (true or false)", agentName)
	}
	return trace, *doc.Bodies, nil
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
