package bootstrap

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/internal/knowledgeproc"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/template"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

const copilotKnowledgeDatasetID = "copilot-reference"

// ensureCoPilotAgent creates the CoPilot agent on first launch and
// (independently) seeds the reference knowledge dataset. The two
// concerns are decoupled on purpose: a previous boot may have created
// the agent successfully but failed mid-seed (no LLM, app crash,
// etc.). Re-running seed on every boot is cheap because
// initCoPilotKnowledge is itself idempotent.
func ensureCoPilotAgent(ctx context.Context, s model.Store, ks knowledge.Store, worker *knowledgeproc.Worker, templateReg *template.Registry) {
	if ks != nil {
		initCoPilotKnowledge(ctx, s, ks, worker)
	}

	if _, err := s.GetAgent(ctx, model.CoPilotAgentID); err == nil {
		telemetry.Info(ctx, "copilot: agent already exists, skipping agent initialization")
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

// initCoPilotKnowledge ingests the embedded reference docs into both
// the app store (so REST consumers can list them) and the knowledge
// store (so retrieval works), and submits each doc to the context
// worker to kick off layered-context generation. The worker's
// debounced rollup then derives the dataset-level L0/L1 in the
// background.
//
// The function is idempotent and re-runs every boot:
//   - the dataset is created on demand
//   - documents already present in the app store are skipped
//     (recoverPendingKnowledgeDocs handles re-submitting unfinished ones)
//   - on ks.AddDocument failure the orphan app row is rolled back so
//     the next boot can retry cleanly
func initCoPilotKnowledge(ctx context.Context, appStore model.Store, ks knowledge.Store, worker *knowledgeproc.Worker) {
	if _, err := appStore.GetDataset(ctx, copilotKnowledgeDatasetID); err != nil {
		if !errdefs.IsNotFound(err) {
			telemetry.Warn(ctx, "copilot: lookup reference dataset failed",
				otellog.String("error", err.Error()))
			return
		}
		if _, err := appStore.CreateDataset(ctx, &model.Dataset{
			ID:          copilotKnowledgeDatasetID,
			Name:        "CoPilot Reference",
			Description: "Built-in reference documents bootstrapped on first launch",
		}); err != nil {
			telemetry.Error(ctx, "copilot: create reference dataset failed",
				otellog.String("error", err.Error()))
			return
		}
	}

	existing, err := appStore.ListDocuments(ctx, copilotKnowledgeDatasetID)
	if err != nil {
		telemetry.Warn(ctx, "copilot: list reference docs failed", otellog.String("error", err.Error()))
		return
	}
	known := make(map[string]struct{}, len(existing))
	for _, d := range existing {
		if d != nil {
			known[d.Name] = struct{}{}
		}
	}

	docs := template.CoPilotReferenceDocs()
	var added int
	for _, doc := range docs {
		if _, ok := known[doc.Name]; ok {
			continue
		}
		row, err := appStore.AddDocument(ctx, copilotKnowledgeDatasetID, doc.Name, doc.Content)
		if err != nil {
			telemetry.Warn(ctx, "copilot: app store add reference doc failed",
				otellog.String("name", doc.Name), otellog.String("error", err.Error()))
			continue
		}
		if err := ks.AddDocument(ctx, copilotKnowledgeDatasetID, doc.Name, doc.Content); err != nil {
			telemetry.Warn(ctx, "copilot: knowledge store add reference doc failed",
				otellog.String("name", doc.Name), otellog.String("error", err.Error()))
			if delErr := appStore.DeleteDocument(ctx, copilotKnowledgeDatasetID, row.ID); delErr != nil {
				telemetry.Warn(ctx, "copilot: rollback app store doc failed",
					otellog.String("name", doc.Name), otellog.String("error", delErr.Error()))
			}
			continue
		}
		added++
		if worker == nil {
			continue
		}
		if err := worker.SubmitDocument(ctx, copilotKnowledgeDatasetID, row.ID, doc.Name, doc.Content); err != nil {
			telemetry.Warn(ctx, "copilot: submit reference doc to context worker failed",
				otellog.String("name", doc.Name), otellog.String("error", err.Error()))
		}
	}
	telemetry.Info(ctx, "copilot: reference knowledge ensured",
		otellog.Int("docs_total", len(docs)),
		otellog.Int("docs_added", added),
		otellog.Bool("worker_enabled", worker != nil))
}

// recoverPendingKnowledgeDocs re-submits documents that were left in
// pending or processing state at startup (typical after a crash mid
// generation). Documents in failed state are intentionally skipped:
// users drive retries explicitly via the reprocess endpoint to avoid
// retry storms when an upstream LLM is genuinely broken.
func recoverPendingKnowledgeDocs(ctx context.Context, appStore model.Store, worker *knowledgeproc.Worker) {
	if worker == nil {
		return
	}
	datasets, err := appStore.ListDatasets(ctx)
	if err != nil {
		telemetry.Warn(ctx, "knowledge: recover list datasets failed", otellog.String("error", err.Error()))
		return
	}
	var resubmitted int
	for _, ds := range datasets {
		if ds == nil {
			continue
		}
		docs, err := appStore.ListDocuments(ctx, ds.ID)
		if err != nil {
			telemetry.Warn(ctx, "knowledge: recover list documents failed",
				otellog.String("dataset", ds.ID), otellog.String("error", err.Error()))
			continue
		}
		for _, d := range docs {
			if d == nil {
				continue
			}
			if d.ProcessingStatus != model.ProcessingPending && d.ProcessingStatus != model.ProcessingRunning {
				continue
			}
			if err := worker.SubmitDocument(ctx, ds.ID, d.ID, d.Name, d.Content); err != nil {
				telemetry.Warn(ctx, "knowledge: recover submit failed",
					otellog.String("dataset", ds.ID),
					otellog.String("doc", d.Name),
					otellog.String("error", err.Error()))
				continue
			}
			resubmitted++
		}
	}
	if resubmitted > 0 {
		telemetry.Info(ctx, "knowledge: recovered unfinished documents",
			otellog.Int("count", resubmitted))
	}
}
