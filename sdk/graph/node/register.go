package node

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
)

func init() {
	RegisterDefaultBuilder("llm", buildLLMNode)
}

func buildLLMNode(def graph.NodeDefinition, bctx *BuildContext) (graph.Node, error) {
	if bctx.LLMResolver == nil {
		return nil, fmt.Errorf("build node %q: LLMResolver is required for llm nodes", def.ID)
	}
	cfg, err := ConfigFromMap(def.Config, variable.ContainsRef)
	if err != nil {
		return nil, fmt.Errorf("build node %q: invalid config: %w", def.ID, err)
	}
	n := NewLLMNode(def.ID, bctx.LLMResolver, bctx.ToolRegistry, cfg)
	n.rawConfig = def.Config
	n.isDeferred = variable.ContainsRef
	return n, nil
}
