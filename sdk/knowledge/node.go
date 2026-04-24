package knowledge

import (
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// DatasetQuery describes a single dataset search within a Knowledge node.
// Re-used by both the v0.3.0 KnowledgeNodeConfig and the deprecated
// KnowledgeConfig (kept stable across versions).
type DatasetQuery struct {
	DatasetID string `json:"dataset_id"`
	StateKey  string `json:"state_key"`
	TopK      int    `json:"top_k"`
}

// KnowledgeNodeConfig is the v0.3.0 graph node configuration.
//
// Field semantics:
//   - Scope     selects whether to search a specific dataset list (Datasets)
//     or every known dataset (ScopeAllDatasets).
//   - Datasets  is consulted only when Scope == ScopeSingleDataset; each
//     entry can override TopK and choose a board key for its hits.
//   - Mode/Layer/TopK/Threshold are forwarded into knowledge.Query.
//
// Backwards-compat shim: KnowledgeNodeConfigFromMap recognises the legacy
// "max_layer" key as "layer" when "layer" is absent. Removed in v0.3.0.
type KnowledgeNodeConfig struct {
	Scope     Scope          `json:"scope,omitempty"`
	Datasets  []DatasetQuery `json:"datasets,omitempty"`
	Mode      Mode           `json:"mode,omitempty"`
	Layer     Layer          `json:"layer,omitempty"`
	TopK      int            `json:"top_k,omitempty"`
	Threshold float64        `json:"threshold,omitempty"`
}

// KnowledgeServiceNode is the v0.3.0 graph node implementation. It
// routes through Service so contract guarantees (Mode/Layer normalisation,
// dataset fan-out, RRF fusion) live in one place.
type KnowledgeServiceNode struct {
	id        string
	svc       *Service
	cfg       KnowledgeNodeConfig
	rawConfig map[string]any
}

// NewKnowledgeServiceNode constructs the v0.3.0 node. svc may be nil
// (search returns empty hits, mirroring the legacy NewKnowledgeNode
// behaviour when store is nil).
func NewKnowledgeServiceNode(id string, svc *Service, cfg KnowledgeNodeConfig) *KnowledgeServiceNode {
	return &KnowledgeServiceNode{id: id, svc: svc, cfg: cfg}
}

// ID implements graph.Node.
func (n *KnowledgeServiceNode) ID() string { return n.id }

// Type implements graph.Node.
func (n *KnowledgeServiceNode) Type() string { return "knowledge" }

// Config implements graph.Node.
func (n *KnowledgeServiceNode) Config() map[string]any { return n.rawConfig }

// SetConfig implements graph.Node.
func (n *KnowledgeServiceNode) SetConfig(c map[string]any) {
	n.rawConfig = c
	n.cfg = KnowledgeNodeConfigFromMap(c)
}

// InputPorts implements graph.Node.
func (n *KnowledgeServiceNode) InputPorts() []graph.Port {
	return []graph.Port{
		{Name: "query", Type: graph.PortTypeString, Required: true},
		{Name: "dataset_id", Type: graph.PortTypeString},
	}
}

// OutputPorts implements graph.Node. "results" is kept as the primary
// output for v0.2.x backwards compatibility; v0.3.0 callers SHOULD
// switch to "hits" (typed []Hit) and the new "by_dataset" projection.
func (n *KnowledgeServiceNode) OutputPorts() []graph.Port {
	return []graph.Port{
		{Name: "hits", Type: graph.PortTypeAny, Required: true},
		{Name: "by_dataset", Type: graph.PortTypeAny},
		{Name: "results", Type: graph.PortTypeAny},
	}
}

// ExecuteBoard implements graph.Node. It runs one Service.Search per
// configured dataset (ScopeSingleDataset) or one global Search across
// every known dataset (ScopeAllDatasets), then publishes hits onto the
// board under the new ("hits", "by_dataset") and legacy ("results")
// keys so existing graphs keep working.
func (n *KnowledgeServiceNode) ExecuteBoard(ectx graph.ExecutionContext, board *graph.Board) error {
	queryVal, _ := board.GetVar("query")
	query := fmt.Sprint(queryVal)

	if n.svc == nil {
		board.SetVar("hits", []Hit{})
		board.SetVar("results", []map[string]any{})
		return nil
	}

	mode := n.cfg.Mode
	layer := n.cfg.Layer
	if layer == "" {
		layer = LayerDetail
	}
	topK := n.cfg.TopK

	if n.cfg.Scope == ScopeAllDatasets {
		hits, err := n.runOne(ectx, query, "", mode, layer, topK)
		if err != nil {
			return err
		}
		n.publishHits(board, hits, hits)
		return nil
	}

	var (
		all       []Hit
		byDataset = map[string][]Hit{}
	)
	datasets := n.cfg.Datasets
	if len(datasets) == 0 {
		// Allow boards that pass dataset_id at runtime: fall back to
		// a single anonymous dataset query when none was configured.
		if id, ok := board.GetVar("dataset_id"); ok {
			datasets = []DatasetQuery{{DatasetID: fmt.Sprint(id), TopK: topK}}
		}
	}
	for _, dq := range datasets {
		dsTopK := dq.TopK
		if dsTopK <= 0 {
			dsTopK = topK
		}
		hits, err := n.runOne(ectx, query, dq.DatasetID, mode, layer, dsTopK)
		if err != nil {
			return err
		}
		stateKey := dq.StateKey
		if stateKey != "" {
			board.SetVar(stateKey, hits)
		}
		byDataset[dq.DatasetID] = hits
		all = append(all, hits...)
	}
	board.SetVar("by_dataset", byDataset)
	n.publishHits(board, all, all)
	return nil
}

// runOne executes one Service.Search and tags telemetry with dataset id.
func (n *KnowledgeServiceNode) runOne(ectx graph.ExecutionContext, query, datasetID string, mode Mode, layer Layer, topK int) ([]Hit, error) {
	ctx, span := telemetry.Tracer().Start(ectx.Context, "node.knowledge.search",
		trace.WithAttributes(attribute.String("dataset_id", datasetID)))
	defer span.End()

	q := Query{
		Text:      query,
		Mode:      mode,
		Layer:     layer,
		TopK:      topK,
		Threshold: n.cfg.Threshold,
	}
	if datasetID == "" {
		q.Scope = ScopeAllDatasets
	} else {
		q.Scope = ScopeSingleDataset
		q.DatasetID = datasetID
	}
	res, err := n.svc.Search(ctx, q)
	if err != nil {
		telemetry.Warn(ctx, "knowledge search failed",
			otellog.String("dataset_id", datasetID),
			otellog.String("error", err.Error()))
		return nil, nil // intentionally swallow per-dataset failure: matches legacy behaviour
	}
	if res == nil {
		return nil, nil
	}
	return res.Hits, nil
}

// publishHits writes the v0.3.0 "hits" ([]Hit) and the legacy
// "results" ([]map[string]any) projections onto the board. Both are
// emitted so graphs that consume either contract keep working.
func (n *KnowledgeServiceNode) publishHits(board *graph.Board, hits, allForLegacy []Hit) {
	board.SetVar("hits", hits)
	board.SetVar("results", hitsToLegacySlice(allForLegacy))
}

// hitsToLegacySlice mirrors the v0.2.x SearchResult JSON shape so
// downstream nodes that consume "results" keep working unchanged.
func hitsToLegacySlice(hits []Hit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"content":  h.Content,
			"score":    h.Score,
			"document": h.DocName,
			"layer":    string(h.Layer),
		})
	}
	return out
}

// KnowledgeNodeConfigFromMap parses map[string]any into KnowledgeNodeConfig.
//
// Backwards compatibility:
//   - "max_layer" is read as "layer" when "layer" is absent.
//   - Numeric "top_k" / "threshold" accept both float64 (JSON) and int.
//   - "scope" accepts "single" (default) and "all".
func KnowledgeNodeConfigFromMap(m map[string]any) KnowledgeNodeConfig {
	cfg := KnowledgeNodeConfig{}
	if v, ok := m["scope"].(string); ok && v == "all" {
		cfg.Scope = ScopeAllDatasets
	}
	if datasets, ok := m["datasets"].([]any); ok {
		for _, d := range datasets {
			if dm, ok := d.(map[string]any); ok {
				cfg.Datasets = append(cfg.Datasets, datasetQueryFromMap(dm))
			}
		}
	}
	if v, ok := m["mode"].(string); ok {
		cfg.Mode = Mode(v)
	}
	switch v := m["layer"].(type) {
	case string:
		if v != "" {
			cfg.Layer = Layer(v)
		}
	}
	if cfg.Layer == "" {
		if v, ok := m["max_layer"].(string); ok && v != "" {
			cfg.Layer = Layer(v)
		}
	}
	if v, ok := numericFromAny(m["top_k"]); ok {
		cfg.TopK = int(v)
	}
	if v, ok := numericFromAny(m["threshold"]); ok {
		cfg.Threshold = v
	}
	return cfg
}

func datasetQueryFromMap(m map[string]any) DatasetQuery {
	dq := DatasetQuery{}
	if v, ok := m["dataset_id"].(string); ok {
		dq.DatasetID = v
	}
	if v, ok := m["state_key"].(string); ok {
		dq.StateKey = v
	}
	if v, ok := numericFromAny(m["top_k"]); ok {
		dq.TopK = int(v)
	}
	return dq
}

// numericFromAny accepts the common JSON-decoded numeric kinds so config
// parsing tolerates both float64 (encoding/json default) and int values
// produced by hand-built map literals.
func numericFromAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// RegisterServiceNode is the v0.3.0 builder registration. It captures
// svc via closure and parses every "knowledge" node config through
// KnowledgeNodeConfigFromMap.
func RegisterServiceNode(svc *Service) {
	node.RegisterDefaultBuilder("knowledge", func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
		cfg := KnowledgeNodeConfigFromMap(def.Config)
		n := NewKnowledgeServiceNode(def.ID, svc, cfg)
		n.rawConfig = def.Config
		return n, nil
	})
}

// KnowledgeServiceNodeSchema returns the v0.3.0 schema. Adds
// "scope" / "mode" / "layer" / "threshold" while keeping legacy fields
// ("max_layer") off the surface; the existing "datasets" field is
// reused (now optional when scope=all).
func KnowledgeServiceNodeSchema() node.NodeSchema {
	return node.NodeSchema{
		Type:        "knowledge",
		Label:       "Knowledge",
		Icon:        "Search",
		Color:       "cyan",
		Category:    "general",
		Description: "Search the knowledge base via Service (v0.3.0).",
		Fields: []node.FieldSchema{
			{Key: "scope", Label: "Scope", Type: "select", DefaultValue: "single",
				Options: []node.SelectOption{
					{Value: "single", Label: "Single dataset(s)"},
					{Value: "all", Label: "All datasets"},
				},
			},
			{
				Key: "datasets", Label: "Datasets", Type: "json",
				Placeholder: `[{"dataset_id": "docs", "state_key": "results", "top_k": 5}]`,
			},
			{Key: "mode", Label: "Mode", Type: "select", DefaultValue: "bm25",
				Options: []node.SelectOption{
					{Value: "bm25", Label: "BM25"},
					{Value: "vector", Label: "Vector"},
					{Value: "hybrid", Label: "Hybrid"},
				},
			},
			{Key: "layer", Label: "Layer", Type: "select", DefaultValue: "L2",
				Options: []node.SelectOption{
					{Value: "L0", Label: "Abstract (L0)"},
					{Value: "L1", Label: "Overview (L1)"},
					{Value: "L2", Label: "Detail (L2)"},
				},
			},
			{Key: "top_k", Label: "Top K", Type: "integer", DefaultValue: 5},
			{Key: "threshold", Label: "Threshold", Type: "number", DefaultValue: 0},
		},
		InputPorts: []node.PortSchema{
			{Name: "query", Type: "string", Required: true},
			{Name: "dataset_id", Type: "string"},
		},
		OutputPorts: []node.PortSchema{
			{Name: "hits", Type: "any", Required: true},
			{Name: "by_dataset", Type: "any"},
			{Name: "results", Type: "any"},
		},
	}
}

// Compile-time assertion.
var _ graph.Node = (*KnowledgeServiceNode)(nil)
