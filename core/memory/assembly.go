package memory

// AssemblyKind is the deployment resource kind of a memory assembly.
// The implementation module (flowcraft memory) registers its factory
// under this kind; core only owns the contract.
const AssemblyKind = "memory.Assembly"

// Assembly is the memory capability contract a deployment binds to:
// agent lifecycle hooks consume it as a context provider and turn
// sink, and applications can push documents. Implementations (such as
// the flowcraft memory module) build one through their config.Factory
// implementation and may expose implementation-specific services
// (like background workers) through their own types; this package
// knows nothing about them.
type Assembly interface {
	ContextProvider
	TurnSink
	DocumentSink
}
