package telemetry

// TraceLevel controls how much of a run is traced.
//
// It is a deployment/host policy, not a property of the code: a durable run
// pays a span per step in exchange for attribution, while an in-memory run of
// cheap nodes would double its cost for spans nobody reads.
type TraceLevel int

const (
	// TraceOff emits no spans. This is the zero value, so an unconfigured run
	// pays nothing.
	TraceOff TraceLevel = iota
	// TraceRun emits one span per run.
	TraceRun
	// TraceStep adds one span per frontier.
	TraceStep
	// TraceNode adds one span per node invocation.
	TraceNode
)

// Allows reports whether a level includes another: a run configured for
// TraceNode also emits the run and step spans.
func (l TraceLevel) Allows(level TraceLevel) bool { return l >= level }
