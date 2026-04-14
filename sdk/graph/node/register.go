package node

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func init() {
	RegisterDefaultBuilder("llm", buildLLMNode)
}

func buildLLMNode(def graph.NodeDefinition, bctx *BuildContext) (graph.Node, error) {
	if bctx.LLMResolver == nil {
		return nil, fmt.Errorf("build node %q: LLMResolver is required for llm nodes", def.ID)
	}
	cfg := ConfigFromMap(def.Config)
	n := NewLLMNode(def.ID, bctx.LLMResolver, bctx.ToolRegistry, cfg)
	n.rawConfig = def.Config
	return n, nil
}
