package engine

// Run is the per-execution input bundle an engine receives alongside
// the host. It is a plain data struct — no methods, no builder, no
// hidden state — assembled once by the host and passed to
// [Engine.Execute] read-only.
//
// All fields are conceptually immutable for the duration of the run.
// Engines may read freely; they MUST NOT mutate the maps in place
// nor mutate the referenced ResumeFrom checkpoint.
type Run struct {
	// ID is a unique identifier for this engine execution. Engines
	// use it as a correlation key in telemetry and may include it in
	// any subjects/headers their host's subject schema requires.
	//
	// The host generates ID and is responsible for keeping it stable
	// across resume / checkpoint cycles.
	ID string

	// Attributes carries arbitrary host-supplied metadata that should
	// flow into telemetry spans and event headers (run id at the
	// agent layer, tenant id, parent span links, engine kind for
	// observability, …).
	//
	// There is intentionally no dedicated field for "engine kind",
	// "run id (agent layer)" or similar: those are observability
	// concerns whose key conventions the host owns. If the consumer
	// side wants to filter by engine kind, the host sets
	// Attributes["engine_kind"] (or whatever key it agreed on).
	Attributes map[string]string

	// Deps is the typed dependency container the host has populated
	// for this run (LLM clients, tool registries, retrievers, …).
	// May be nil if the engine needs no dependencies; engines that
	// look up dependencies should use [GetDep] which handles nil.
	Deps *Dependencies

	// ResumeFrom, when non-nil, instructs the engine to continue
	// execution from the provided checkpoint instead of starting a
	// fresh run. The engine is the sole interpreter of
	// [Checkpoint.Step] and [Checkpoint.Payload]; the host treats
	// them as opaque.
	//
	// Contract:
	//
	//   - When ResumeFrom is non-nil the engine SHOULD prefer
	//     ResumeFrom.Board over the board parameter passed to
	//     [Engine.Execute]; passing both is allowed but the
	//     checkpoint's board takes precedence as it represents the
	//     state at the boundary the run paused on.
	//
	//   - When ResumeFrom.ExecID differs from [Run.ID] the engine MUST
	//     return an errdefs.Validation-classified error: forking a
	//     run requires a fresh execution id, not a resume.
	//
	//   - Engines that do not support resume MUST return an
	//     errdefs.NotAvailable-classified error when they observe a
	//     non-nil ResumeFrom rather than silently restarting from
	//     scratch.
	//
	// Hosts that drive resumption typically [CheckpointStore.Load]
	// the most recent checkpoint, set ResumeFrom, and call
	// [Engine.Execute] again with the same Run.ID.
	ResumeFrom *Checkpoint
}

// Attribute returns the value for the given attribute key, or "" if
// absent. A small convenience over `r.Attributes[key]` that handles
// a nil Attributes map.
func (r Run) Attribute(key string) string {
	if r.Attributes == nil {
		return ""
	}
	return r.Attributes[key]
}
