package agent

// KindExtension is the one kind every engine-defined event reports.
//
// The vocabulary stays closed: a consumer switches over Kind once, and the
// engine-specific taxonomy lives inside the payload it chose. That is the same
// trade Record makes for durable facts - a framework that stays still while
// extensions speak.
const KindExtension EventKind = "extension"

// EventExtension is an event whose payload belongs to the engine that emitted it.
//
// The type parameter is the payload: an engine names its own struct, so both ends
// are typed where it matters - the emitter constructs it, and a consumer that knows
// the engine switches on the instantiation. A consumer that does not know the
// payload uses [Extension], which is the same event with the type erased.
type EventExtension[T any] struct {
	// Name is the emitter's name for the payload ("graph.state"), for the readers
	// that never see the type: a log, a trace, a protocol adapter.
	Name string
	// Value is the payload itself.
	Value T
}

// Kind implements [Event]. Every extension reports the same kind.
func (EventExtension[T]) Kind() EventKind { return KindExtension }

func (EventExtension[T]) event() {}

// Extension is the type-erased view of an [EventExtension].
//
// Without it an engine's event would be invisible to every layer that does not
// import that engine - and a logger that can only print "extension" is not a
// logger. The payload crosses as any, so its cost is the cost of boxing it: keep
// extension payloads small, and put bulk behind a reference.
type Extension interface {
	Event
	// ExtensionName is the emitter's name for the payload.
	ExtensionName() string
	// ExtensionValue is the payload, as the emitter built it.
	ExtensionValue() any
}

// ExtensionName implements [Extension].
func (e EventExtension[T]) ExtensionName() string { return e.Name }

// ExtensionValue implements [Extension].
func (e EventExtension[T]) ExtensionValue() any { return e.Value }
