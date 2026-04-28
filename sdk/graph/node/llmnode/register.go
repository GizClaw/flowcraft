package llmnode

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/variable"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Register binds the "llm" node builder onto factory. The resolver argument
// is required; toolReg may be nil if no tool-calling is configured.
func Register(factory *node.Factory, resolver llm.LLMResolver, toolReg *tool.Registry) {
	factory.RegisterBuilder("llm", func(def graph.NodeDefinition) (graph.Node, error) {
		if resolver == nil {
			return nil, fmt.Errorf("build node %q: LLMResolver is required for llm nodes", def.ID)
		}
		cfg, err := ConfigFromMap(def.Config, variable.ContainsRef)
		if err != nil {
			return nil, fmt.Errorf("build node %q: invalid config: %w", def.ID, err)
		}
		n := New(def.ID, resolver, toolReg, cfg)
		n.rawConfig = def.Config
		n.isDeferred = variable.ContainsRef
		return n, nil
	})
}
