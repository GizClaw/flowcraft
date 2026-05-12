package agent

// runinfo_attrs.go owns the contract between [Run] (which promotes
// per-call identity into [engine.Run.Attributes]) and downstream
// readers that need to reconstruct a [RunInfo] from those attributes.
//
// The string keys are deliberately kept private to the agent package:
// callers never type the key strings themselves; they go through
// [RunInfoFromAttributes] (read side) or rely on Run's promotion (write
// side). That way migrating the wire format (e.g. switching to
// telemetry.Attr* canonical dot-keys) is a one-edit change here, not a
// codebase-wide grep-and-replace.
const (
	// attrAgentID — agent.Agent.ID promoted by Run into engine.Run.Attributes.
	attrAgentID = "agent_id"

	// attrRunID — the engine.Run.ID promoted by Run. Engines that key
	// telemetry off engine.Run.ID directly do NOT need to read this;
	// it exists so a node looking at engine.Run.Attributes alone can
	// recover the full RunInfo without consulting engine.Run.
	attrRunID = "run_id"

	// attrTaskID / attrContextID — Request.TaskID / Request.ContextID
	// (A2A-aligned identifiers) promoted by Run when non-empty.
	attrTaskID    = "task_id"
	attrContextID = "context_id"
)

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
		AgentID:   attrs[attrAgentID],
		RunID:     runID,
		TaskID:    attrs[attrTaskID],
		ContextID: attrs[attrContextID],
	}
}
