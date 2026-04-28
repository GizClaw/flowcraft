package knowledgenode

import (
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

// Register binds the "knowledge" node builder onto factory. svc may be nil
// (the node will return empty hits at execution time).
func Register(factory *node.Factory, svc *knowledge.Service) {
	factory.RegisterBuilder("knowledge", func(def graph.NodeDefinition) (graph.Node, error) {
		cfg := ConfigFromMap(def.Config)
		n := New(def.ID, svc, cfg)
		n.rawConfig = def.Config
		return n, nil
	})
}
