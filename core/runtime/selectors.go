package runtime

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/goccy/go-json"
)

// SelectorFactory builds a selector from its written settings.
type SelectorFactory[Req any] func(settings json.RawMessage) (route.Selector[Req], error)

// Selector names a selection strategy so YAML can refer to it.
//
// Two typed slots rather than one because route.Selector[GenerateRequest] and
// route.Selector[DecisionRequest] are different Go types: one factory cannot
// produce both. A strategy that only matters for generation leaves Decision nil,
// and judgment then keeps the declaration order.
//
// Pure Go callers need none of this: they pass their own Selector value to
// route.Policy. The table exists only to give a name to something YAML cannot
// express.
type Selector struct {
	// Name is the value written in inference.route.selector.
	Name string
	// Generate builds the generation selector.
	Generate SelectorFactory[inference.GenerateRequest]
	// Decision builds the judgment selector.
	Decision SelectorFactory[inference.DecisionRequest]
}

func selectorTable(selectors []Selector) (map[string]Selector, error) {
	table := make(map[string]Selector, len(selectors))
	for _, selector := range selectors {
		if selector.Name == "" {
			return nil, errorf("runtime: a selector has no name")
		}
		if selector.Generate == nil && selector.Decision == nil {
			return nil, errorf("runtime: selector %q builds neither seam", selector.Name)
		}
		if _, exists := table[selector.Name]; exists {
			return nil, errorf("runtime: selector %q is registered twice", selector.Name)
		}
		table[selector.Name] = selector
	}
	return table, nil
}
