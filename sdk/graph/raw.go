package graph

// RawGraph is the intermediate graph structure produced by the compiler.
// All fields are exported for static analysis during compilation.
// It is NOT intended for direct execution — use Assemble to produce an
// immutable *Graph for the executor.
type RawGraph struct {
	Name           string
	Entry          string
	Nodes          map[string]Node
	Edges          map[string][]Edge
	Reverse        map[string][]string
	SkipConditions map[string]*CompiledCondition
}
