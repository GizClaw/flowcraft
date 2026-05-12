package agent

import "github.com/GizClaw/flowcraft/sdk/telemetry"

// runinfo_attrs.go owns the contract between [Run] (which promotes
// per-call identity into [engine.Run.Attributes]) and downstream
// readers that need to reconstruct a [RunInfo] from those attributes.
//
// The wire format is the canonical OpenTelemetry-style dot-key set
// defined in sdk/telemetry/attrs.go (AttrAgentID, AttrRunID,
// AttrTaskID, AttrConversationID). Routing identity through the
// telemetry catalog instead of agent-private snake_case constants
// gives downstream consumers (executor agent_id resolver, dashboards,
// future sdk/pod controller) one canonical key set to filter on,
// closing the contract-audit #15 gap where the executor had no
// process-portable channel for agent.id.
//
// The mapping ContextID → AttrConversationID reflects A2A semantics:
// "context" / "conversation" name the same thread-of-interaction
// scope; collapsing onto one canonical key avoids dashboard joins
// having to know about both spellings.

// RunInfoFromAttributes reconstructs a [RunInfo] from the
// engine.Run.Attributes map [Run] populates on every attempt. runID
// is taken from the caller-supplied argument (typically
// engine.Run.ID, the canonical source) rather than the attribute
// copy, since some downstream contexts (e.g. graph.ExecutionContext)
// expose RunID as a dedicated field separate from the attributes
// bag.
//
// Missing keys yield empty strings — RunInfoFromAttributes never
// errors. This matches Run's "promote when non-empty" write policy:
// a missing key just means the upstream Request did not carry that
// identifier, not that something went wrong.
//
// Typical caller (a graph node bridging RunInfo into a script
// runtime):
//
//	info := agent.RunInfoFromAttributes(ec.RunID, ec.Attributes)
//	bindings.NewRunInfoBridge(info)
//
// This closes contract-audit #12: nodes used to construct
// RunInfo{RunID: ec.RunID} verbatim, leaving AgentID / TaskID /
// ContextID empty even though Run had written them upstream into
// engine.Run.Attributes.
func RunInfoFromAttributes(runID string, attrs map[string]string) RunInfo {
	return RunInfo{
		AgentID:   attrs[telemetry.AttrAgentID],
		RunID:     runID,
		TaskID:    attrs[telemetry.AttrTaskID],
		ContextID: attrs[telemetry.AttrConversationID],
	}
}
