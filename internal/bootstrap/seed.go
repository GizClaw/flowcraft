package bootstrap

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/template"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

func ensureCoPilotAgent(ctx context.Context, s model.Store, ks knowledge.Store, templateReg *template.Registry) {
	_, err := s.GetAgent(ctx, model.CoPilotAgentID)
	if err == nil {
		telemetry.Info(ctx, "copilot: agent already exists, skipping initialization")
		return
	}

	telemetry.Info(ctx, "copilot: creating CoPilot agents (first-time initialization)")

	// Create sub-agents first so we have their IDs.
	// Name and Description are read from the template (single source of truth).
	subAgentDefs := []struct {
		ID           string
		TemplateName string
	}{
		{"copilot-builder", "copilot_builder"},
	}

	subAgentIDs := make([]string, 0, len(subAgentDefs))
	for _, def := range subAgentDefs {
		tmpl, graphDef := resolveTemplate(ctx, templateReg, def.TemplateName, def.ID)
		agent := &model.Agent{
			AgentID:     def.ID,
			Name:        tmpl.Label,
			Type:        model.AgentTypeCoPilot,
			Description: tmpl.Description,
			StrategyDef: model.NewGraphStrategy(graphDef),
			Config: model.AgentConfig{
				Memory: model.MemoryConfig{
					Type: "lossless",
					Lossless: model.LosslessConfig{
						TokenBudget: 4000,
						ChunkSize:   8,
					},
				},
			},
		}
		if _, err := s.CreateAgent(ctx, agent); err != nil {
			telemetry.Error(ctx, "copilot: failed to create sub-agent",
				otellog.String("id", def.ID), otellog.String("error", err.Error()))
			continue
		}
		subAgentIDs = append(subAgentIDs, def.ID)
		telemetry.Info(ctx, "copilot: sub-agent created", otellog.String("id", def.ID))
	}

	// Create Dispatcher agent
	_, dispatcherGraphDef := resolveTemplate(ctx, templateReg, "copilot_dispatcher", "copilot")
	copilotAgent := &model.Agent{
		AgentID:     model.CoPilotAgentID,
		Name:        "CoPilot",
		Type:        model.AgentTypeCoPilot,
		Description: "Built-in AI assistant for workflow orchestration",
		StrategyDef: model.NewGraphStrategy(dispatcherGraphDef),
		Config: model.AgentConfig{
			SubAgents: subAgentIDs,
			Memory: model.MemoryConfig{
				Type: "lossless",
				Lossless: model.LosslessConfig{
					TokenBudget: 8000,
					ChunkSize:   10,
				},
				LongTerm: model.LongTermConfig{
					Enabled:    true,
					Categories: []string{"cases", "patterns"},
					MaxEntries: 200,
				},
			},
		},
	}

	created, err := s.CreateAgent(ctx, copilotAgent)
	if err != nil {
		telemetry.Error(ctx, "copilot: failed to create dispatcher agent", otellog.String("error", err.Error()))
		return
	}
	telemetry.Info(ctx, "copilot: dispatcher agent created", otellog.String("id", created.AgentID))

	if ks != nil {
		initCoPilotKnowledge(ctx, ks)
	}
}

// resolveTemplate returns the template metadata and the instantiated GraphDefinition.
// If the template is not found, it returns a zero-value GraphTemplate and nil.
func resolveTemplate(ctx context.Context, templateReg *template.Registry, templateName, graphName string) (template.GraphTemplate, *model.GraphDefinition) {
	if templateReg == nil {
		return template.GraphTemplate{}, nil
	}
	t, ok := templateReg.Get(templateName)
	if !ok {
		return template.GraphTemplate{}, nil
	}
	instantiated, err := template.Instantiate(t, nil)
	if err != nil {
		telemetry.Warn(ctx, "copilot: failed to instantiate template",
			otellog.String("template", templateName), otellog.String("error", err.Error()))
		return t, nil
	}
	raw, _ := json.Marshal(instantiated)
	var gd model.GraphDefinition
	if json.Unmarshal(raw, &gd) == nil {
		if gd.Name == "" {
			gd.Name = graphName
		}
		return t, &gd
	}
	return t, nil
}

func initCoPilotKnowledge(ctx context.Context, ks knowledge.Store) {
	const datasetID = "copilot-reference"
	docs := template.CoPilotReferenceDocs()
	for _, doc := range docs {
		if err := ks.AddDocument(ctx, datasetID, doc.Name, doc.Content); err != nil {
			telemetry.Warn(ctx, "copilot: failed to add reference doc",
				otellog.String("name", doc.Name), otellog.String("error", err.Error()))
		}
	}
	telemetry.Info(ctx, "copilot: reference knowledge initialized", otellog.Int("docs", len(docs)))
}
