package knowledgenode

import (
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// Config configures a knowledge graph node.
//
// Field semantics:
//   - Scope     selects whether to search a specific dataset list (Datasets)
//     or every known dataset (knowledge.ScopeAllDatasets).
//   - Datasets  is consulted only when Scope == knowledge.ScopeSingleDataset;
//     each entry can override TopK and choose a board key for its hits.
//   - Mode/Layer/TopK/Threshold are forwarded into knowledge.Query.
type Config struct {
	Scope     knowledge.Scope          `json:"scope,omitempty"`
	Datasets  []knowledge.DatasetQuery `json:"datasets,omitempty"`
	Mode      knowledge.Mode           `json:"mode,omitempty"`
	Layer     knowledge.Layer          `json:"layer,omitempty"`
	TopK      int                      `json:"top_k,omitempty"`
	Threshold float64                  `json:"threshold,omitempty"`
}

// Node is a graph node that retrieves documents from a knowledge.Service.
// It delegates to Service so contract guarantees (Mode/Layer normalisation,
// dataset fan-out, RRF fusion) live in one place.
type Node struct {
	id        string
	svc       *knowledge.Service
	cfg       Config
	rawConfig map[string]any
}

// New constructs a knowledge node. svc may be nil — in that case ExecuteBoard
// writes empty hits to the board instead of failing.
func New(id string, svc *knowledge.Service, cfg Config) *Node {
	return &Node{id: id, svc: svc, cfg: cfg}
}

// ID implements graph.Node.
func (n *Node) ID() string { return n.id }

// Type implements graph.Node.
func (n *Node) Type() string { return "knowledge" }

// Config implements graph.Node.
func (n *Node) Config() map[string]any { return n.rawConfig }

// SetConfig implements graph.Node.
func (n *Node) SetConfig(c map[string]any) {
	n.rawConfig = c
	n.cfg = ConfigFromMap(c)
}

// InputPorts implements graph.Node.
func (n *Node) InputPorts() []graph.Port {
	return []graph.Port{
		{Name: "query", Type: graph.PortTypeString, Required: true},
		{Name: "dataset_id", Type: graph.PortTypeString},
	}
}

// OutputPorts implements graph.Node. New callers should consume "hits"
// (typed []knowledge.Hit) and "by_dataset"; "results" carries the same
// content projected into []map[string]any for compatibility with older
// graphs that consume that key.
func (n *Node) OutputPorts() []graph.Port {
	return []graph.Port{
		{Name: "hits", Type: graph.PortTypeAny, Required: true},
		{Name: "by_dataset", Type: graph.PortTypeAny},
		{Name: "results", Type: graph.PortTypeAny},
	}
}

// ExecuteBoard implements graph.Node. It runs one Service.Search per
// configured dataset (ScopeSingleDataset) or one global Search across
// every known dataset (ScopeAllDatasets), then publishes the hits onto
// the board under "hits" / "by_dataset" / "results".
func (n *Node) ExecuteBoard(ectx graph.ExecutionContext, board *graph.Board) error {
	queryVal, _ := board.GetVar("query")
	query := fmt.Sprint(queryVal)

	if n.svc == nil {
		board.SetVar("hits", []knowledge.Hit{})
		board.SetVar("results", []map[string]any{})
		return nil
	}

	mode := n.cfg.Mode
	layer := n.cfg.Layer
	if layer == "" {
		layer = knowledge.LayerDetail
	}
	topK := n.cfg.TopK

	if n.cfg.Scope == knowledge.ScopeAllDatasets {
		hits, err := n.runOne(ectx, query, "", mode, layer, topK)
		if err != nil {
			return err
		}
		n.publishHits(board, hits, hits)
		return nil
	}

	var (
		all       []knowledge.Hit
		byDataset = map[string][]knowledge.Hit{}
	)
	datasets := n.cfg.Datasets
	if len(datasets) == 0 {
		// Allow boards that pass dataset_id at runtime: fall back to
		// a single anonymous dataset query when none was configured.
		if id, ok := board.GetVar("dataset_id"); ok {
			datasets = []knowledge.DatasetQuery{{DatasetID: fmt.Sprint(id), TopK: topK}}
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
func (n *Node) runOne(ectx graph.ExecutionContext, query, datasetID string, mode knowledge.Mode, layer knowledge.Layer, topK int) ([]knowledge.Hit, error) {
	ctx, span := telemetry.Tracer().Start(ectx.Context, "node.knowledge.search",
		trace.WithAttributes(attribute.String(telemetry.AttrDatasetID, datasetID)))
	defer span.End()

	q := knowledge.Query{
		Text:      query,
		Mode:      mode,
		Layer:     layer,
		TopK:      topK,
		Threshold: n.cfg.Threshold,
	}
	if datasetID == "" {
		q.Scope = knowledge.ScopeAllDatasets
	} else {
		q.Scope = knowledge.ScopeSingleDataset
		q.DatasetID = datasetID
	}
	res, err := n.svc.Search(ctx, q)
	if err != nil {
		telemetry.Warn(ctx, "knowledge search failed",
			otellog.String(telemetry.AttrDatasetID, datasetID),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		// Intentionally swallow per-dataset failure so one bad dataset
		// does not abort the whole fan-out; failed datasets contribute
		// zero hits and the remaining datasets still surface results.
		return nil, nil
	}
	if res == nil {
		return nil, nil
	}
	return res.Hits, nil
}

// publishHits writes both projections of the search results onto the board:
//   - "hits"    typed []knowledge.Hit (preferred for new graphs)
//   - "results" []map[string]any      (compat projection consumed by graphs
//     that were authored before the typed hits API existed)
func (n *Node) publishHits(board *graph.Board, hits, allForCompat []knowledge.Hit) {
	board.SetVar("hits", hits)
	board.SetVar("results", hitsToCompatSlice(allForCompat))
}

// hitsToCompatSlice mirrors the older SearchResult JSON shape so downstream
// nodes that read "results" keep working unchanged.
func hitsToCompatSlice(hits []knowledge.Hit) []map[string]any {
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

// ConfigFromMap parses map[string]any into Config. It tolerates a few
// surface-shape variations so hand-written and JSON-decoded configs both
// round-trip:
//   - "max_layer" is accepted as an alias for "layer" when "layer" is absent.
//   - Numeric "top_k" / "threshold" accept both float64 (JSON) and int.
//   - "scope" accepts "single" (default) and "all".
func ConfigFromMap(m map[string]any) Config {
	cfg := Config{}
	if v, ok := m["scope"].(string); ok && v == "all" {
		cfg.Scope = knowledge.ScopeAllDatasets
	}
	if datasets, ok := m["datasets"].([]any); ok {
		for _, d := range datasets {
			if dm, ok := d.(map[string]any); ok {
				cfg.Datasets = append(cfg.Datasets, datasetQueryFromMap(dm))
			}
		}
	}
	if v, ok := m["mode"].(string); ok {
		cfg.Mode = knowledge.Mode(v)
	}
	switch v := m["layer"].(type) {
	case string:
		if v != "" {
			cfg.Layer = knowledge.Layer(v)
		}
	}
	if cfg.Layer == "" {
		if v, ok := m["max_layer"].(string); ok && v != "" {
			cfg.Layer = knowledge.Layer(v)
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

func datasetQueryFromMap(m map[string]any) knowledge.DatasetQuery {
	dq := knowledge.DatasetQuery{}
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

// Compile-time assertion.
var _ graph.Node = (*Node)(nil)
