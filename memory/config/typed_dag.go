package config

import (
	"fmt"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/lifecycle"
	"github.com/GizClaw/flowcraft/memory/lines/chat"
	"github.com/GizClaw/flowcraft/memory/lines/knowledge"
)

func (settings Settings) chatSpec() (derive.Spec, error) {
	nodes := make([]derive.NodeSpec, len(settings.ChatDAG.Nodes))
	for index, configured := range settings.ChatDAG.Nodes {
		selected := 0
		var factory component.DeriverSpec
		if configured.Fact != nil {
			selected++
			factory = component.NewDeriverSpec("chat.fact", *configured.Fact)
		}
		if configured.custom != nil {
			selected++
			factory = *configured.custom
		}
		if selected != 1 {
			return derive.Spec{}, fmt.Errorf("memory config: chat_dag.nodes[%d] must select exactly one typed algorithm", index)
		}
		nodes[index] = derive.NodeSpec{
			ID: configured.ID, Deriver: factory, DependsOn: append([]string(nil), configured.DependsOn...),
		}
	}
	return derive.Spec{SourceKinds: []component.ArtifactKind{chat.KindRawMessage}, Nodes: nodes}, nil
}

func (settings Settings) knowledgeSpec() (derive.Spec, error) {
	nodes := make([]derive.NodeSpec, len(settings.KnowledgeDAG.Nodes))
	for index, configured := range settings.KnowledgeDAG.Nodes {
		selected := 0
		var factory component.DeriverSpec
		if configured.Chunk != nil {
			selected++
			factory = component.NewDeriverSpec("knowledge.chunk", *configured.Chunk)
		}
		if configured.custom != nil {
			selected++
			factory = *configured.custom
		}
		if selected != 1 {
			return derive.Spec{}, fmt.Errorf("memory config: knowledge_dag.nodes[%d] must select exactly one typed algorithm", index)
		}
		nodes[index] = derive.NodeSpec{
			ID: configured.ID, Deriver: factory, DependsOn: append([]string(nil), configured.DependsOn...),
		}
	}
	return derive.Spec{SourceKinds: []component.ArtifactKind{knowledge.KindDocument}, Nodes: nodes}, nil
}

func (settings Settings) lifecycleDAG() (*lifecycle.DAG, error) {
	nodes := make([]lifecycle.NodeSpec, len(settings.LifecycleDAG.Nodes))
	for index, configured := range settings.LifecycleDAG.Nodes {
		selected := 0
		node := lifecycle.NodeSpec{
			ID: configured.ID, Phase: configured.Phase,
			DependsOn: append([]string(nil), configured.DependsOn...),
		}
		if configured.Phase != "" {
			selected++
		}
		if configured.custom != nil {
			selected++
			node.Factory = *configured.custom
		}
		if selected != 1 {
			return nil, fmt.Errorf("memory config: lifecycle_dag.nodes[%d] must select exactly one built-in phase or typed factory", index)
		}
		nodes[index] = node
	}
	var available []lifecycle.StateKind
	if settings.LifecycleEffects != nil {
		available = append(available, lifecycle.StateEffectSink)
	}
	return lifecycle.Build(settings.LifecycleFactoryCatalog, lifecycle.Spec{Nodes: nodes}, available...)
}
