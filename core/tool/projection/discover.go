package projection

// DiscoverResult reports one tool's discovery outcome.
type DiscoverResult struct {
	Name string `json:"name"`
	// Exposed reports whether the tool will appear in the next Definitions call:
	// pooled and within the per-round budget.
	Exposed bool `json:"exposed"`
	// Reason explains a name that is not exposed:
	// "unknown" | "not_discoverable" | "over_budget" | "visible_budget".
	Reason string `json:"reason,omitempty"`
}

// DiscoverOutcome summarizes one Discover call.
type DiscoverOutcome struct {
	// Results carries one entry per requested name, in input order.
	Results []DiscoverResult `json:"results"`
	// Evicted lists pool entries dropped by budget pressure (LRU) while fitting the
	// newly discovered names.
	Evicted []string `json:"evicted,omitempty"`
}
