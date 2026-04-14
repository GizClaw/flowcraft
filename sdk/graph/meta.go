package graph

// GraphMeta contains structural analysis results produced by the compiler.
type GraphMeta struct {
	NodeCount   int  `json:"node_count"`
	EdgeCount   int  `json:"edge_count"`
	HasCycles   bool `json:"has_cycles"`
	HasParallel bool `json:"has_parallel"`
	MaxDepth    int  `json:"max_depth"`
}
