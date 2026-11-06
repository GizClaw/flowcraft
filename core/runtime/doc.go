// Package runtime assembles one agent runtime from a configuration document and
// owns everything it built.
//
// The unit is deliberately not a session. A Runtime holds the substrate a turn
// runs on — the model provider, the tool registry with its instances, the
// sandbox runner, the file boundary, the skill index, the journal — and nothing
// that belongs to a conversation: no history, no turn counter, no per-user
// state. Two Runtimes can live in one process without sharing anything, which
// is what makes "atomic" true rather than aspirational. History is an argument
// to a turn, and the turn loop itself belongs to whatever layer adds prompting.
//
// Ownership has one rule: a Runtime closes what it created. A value handed in
// through an option is somebody else's to close, and [Runtime.Close] leaves it
// alone. Close runs in reverse construction order — tools, then journal, then
// runner, then tree — because a session that outlives its runner writes into a
// corpse, and a journal that outlives its runner drops the last events.
//
// The configuration is YAML, parsed into [Config]: a set of mirror types that
// hold the human-facing units (mebibytes, mode names, paths) and convert to the
// contract types. The contracts carry no serialization tags, so the wire format
// of a policy is not part of its public API. Layers let a deployment stack a
// base file, a profile and a local override; see [LoadLayers].
package runtime
