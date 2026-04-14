package knowledge

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// KnowledgeConfig configures a Knowledge node.
type KnowledgeConfig struct {
	Datasets []DatasetQuery `json:"datasets"`
	MaxLayer ContextLayer   `json:"max_layer,omitempty"` // L0, L1, L2 (default)
}

// DatasetQuery describes a single dataset search within a Knowledge node.
type DatasetQuery struct {
	DatasetID string `json:"dataset_id"`
	StateKey  string `json:"state_key"`
	TopK      int    `json:"top_k"`
}

// KnowledgeNode is a Go-native graph node for knowledge retrieval.
type KnowledgeNode struct {
	id        string
	store     Store
	config    KnowledgeConfig
	rawConfig map[string]any
}

// NewKnowledgeNode creates a Knowledge node. store may be nil (retrieval returns empty).
func NewKnowledgeNode(id string, store Store, config KnowledgeConfig) *KnowledgeNode {
	return &KnowledgeNode{id: id, store: store, config: config}
}

func (n *KnowledgeNode) ID() string   { return n.id }
func (n *KnowledgeNode) Type() string { return "knowledge" }

func (n *KnowledgeNode) Config() map[string]any { return n.rawConfig }
func (n *KnowledgeNode) SetConfig(c map[string]any) {
	n.rawConfig = c
	n.config = KnowledgeConfigFromMap(c)
}

func (n *KnowledgeNode) InputPorts() []graph.Port {
	return []graph.Port{
		{Name: "query", Type: graph.PortTypeString, Required: true},
	}
}

func (n *KnowledgeNode) OutputPorts() []graph.Port {
	return []graph.Port{
		{Name: "results", Type: graph.PortTypeAny, Required: true},
	}
}

func (n *KnowledgeNode) ExecuteBoard(ectx graph.ExecutionContext, board *graph.Board) error {
	queryVal, ok := board.GetVar("query")
	if !ok {
		queryVal = ""
	}
	query := fmt.Sprint(queryVal)

	if n.store == nil {
		board.SetVar("results", []map[string]any{})
		return nil
	}

	maxLayer := n.config.MaxLayer
	if maxLayer == "" {
		maxLayer = LayerDetail
	}

	var allResults []map[string]any
	for _, dq := range n.config.Datasets {
		ctx, span := telemetry.Tracer().Start(ectx.Context, "node.knowledge.search",
			trace.WithAttributes(attribute.String("dataset_id", dq.DatasetID)))

		topK := dq.TopK
		if topK <= 0 {
			topK = 5
		}

		results, err := n.store.Search(ctx, dq.DatasetID, query, SearchOptions{
			TopK:     topK,
			MaxLayer: maxLayer,
		})
		span.End()

		if err != nil {
			telemetry.Warn(ctx, "knowledge search failed",
				otellog.String("dataset_id", dq.DatasetID),
				otellog.String("error", err.Error()))
			continue
		}

		items := searchResultsToSlice(results)
		stateKey := dq.StateKey
		if stateKey == "" {
			stateKey = "results"
		}
		board.SetVar(stateKey, items)
		allResults = append(allResults, items...)
	}

	board.SetVar("results", allResults)
	return nil
}

func searchResultsToSlice(results []SearchResult) []map[string]any {
	items := make([]map[string]any, 0, len(results))
	for _, r := range results {
		items = append(items, map[string]any{
			"content":  r.Content,
			"score":    r.Score,
			"document": r.DocName,
			"layer":    string(r.Layer),
		})
	}
	return items
}

// KnowledgeConfigFromMap parses a KnowledgeConfig from a generic map.
func KnowledgeConfigFromMap(m map[string]any) KnowledgeConfig {
	cfg := KnowledgeConfig{}
	if datasets, ok := m["datasets"].([]any); ok {
		for _, d := range datasets {
			if dm, ok := d.(map[string]any); ok {
				dq := DatasetQuery{}
				if v, ok := dm["dataset_id"].(string); ok {
					dq.DatasetID = v
				}
				if v, ok := dm["state_key"].(string); ok {
					dq.StateKey = v
				}
				if v, ok := dm["top_k"].(float64); ok {
					dq.TopK = int(v)
				}
				cfg.Datasets = append(cfg.Datasets, dq)
			}
		}
	}
	if v, ok := m["max_layer"].(string); ok {
		cfg.MaxLayer = ContextLayer(v)
	}
	return cfg
}

// RegisterNode registers the "knowledge" node builder with the SDK's node
// factory, capturing the given Store via closure. Call this from bootstrap
// before constructing the node factory.
func RegisterNode(ks Store) {
	node.RegisterDefaultBuilder("knowledge", func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
		cfg := KnowledgeConfigFromMap(def.Config)
		n := NewKnowledgeNode(def.ID, ks, cfg)
		n.rawConfig = def.Config
		return n, nil
	})
}

// KnowledgeNodeSchema returns the NodeSchema for the knowledge node type,
// used by the schema registry for frontend metadata.
func KnowledgeNodeSchema() node.NodeSchema {
	return node.NodeSchema{
		Type:        "knowledge",
		Label:       "Knowledge",
		Icon:        "Search",
		Color:       "cyan",
		Category:    "general",
		Description: "Search knowledge base and retrieve relevant documents",
		Fields: []node.FieldSchema{
			{
				Key: "datasets", Label: "Datasets", Type: "json", Required: true,
				Placeholder: `[{"dataset_id": "docs", "state_key": "results", "top_k": 5}]`,
			},
			{Key: "max_layer", Label: "Max Layer", Type: "select", DefaultValue: "L2",
				Options: []node.SelectOption{
					{Value: "L0", Label: "Abstract (L0)"},
					{Value: "L1", Label: "Overview (L1)"},
					{Value: "L2", Label: "Detail (L2)"},
				},
			},
		},
		InputPorts: []node.PortSchema{
			{Name: "query", Type: "string", Required: true},
		},
		OutputPorts: []node.PortSchema{
			{Name: "results", Type: "any", Required: true},
		},
	}
}
