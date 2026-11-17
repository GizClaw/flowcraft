package correlation

// Identity attribute keys.
//
// These names live here, beside the carrier that renders them, rather than in
// core/telemetry: they describe *who* is doing the work, they are produced only
// by [Run] (or by a host that sets one field by hand), and every producer that
// needs them already imports this package to attach the run. Keeping them in
// telemetry made this package depend on the observability layer for nothing but
// seven strings, which is what forced it under core/runtime in the first place.
//
// The keys are string constants, not attribute.Key values: a consumer that only
// wants the canonical name (a log field, an envelope header) must not be forced
// to import the OTel SDK. See core/telemetry/attrs.go for the keys that describe
// the work rather than its identity.
//
// Renaming any of these is a breaking change for every dashboard, alert and log
// query that filters on the key; [TestAttrNamesAreStable] pins them.
const (
	// AttrRuntimeID identifies one assembled agent runtime (core/runtime.Runtime.ID):
	// the substrate a turn runs on, not a conversation. Two runtimes in one
	// process have different ids, so this is what separates their spans. It is
	// distinct from agent.id, which names the logical agent that may outlive or
	// share a runtime.
	AttrRuntimeID = "agent.runtime.id"

	// AttrTenantID identifies the tenant on whose behalf the work is running.
	// Producers populate it when a tenant boundary is meaningful (multi-tenant
	// SaaS deployments).
	AttrTenantID = "tenant.id"

	// AttrSessionID identifies the session an operation belongs to: the
	// container of tasks, which outlives each of them.
	AttrSessionID = "session.id"

	// AttrAgentName names the deployed agent within a multi-agent host.
	AttrAgentName = "agent.name"

	// AttrTurn is the conversation turn a step belongs to.
	AttrTurn = "turn"

	// AttrManifestID identifies the deployment manifest a run executed under.
	AttrManifestID = "agent.manifest.id"

	// AttrTaskID identifies the A2A-aligned task an operation belongs to. It is
	// the checkpoint key, stable across a resume, and empty when the layer that
	// attached the run does not know it yet.
	AttrTaskID = "task.id"
)
