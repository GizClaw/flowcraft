package knowledge

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// defaultDatasetID is the implicit dataset for tools that omit dataset_id.
const defaultDatasetID = "default"

// NewSearchServiceTool exposes [knowledge.Service.Search] to LLMs.
//
// Tool name:  "knowledge_search"
// Input JSON:
//
//	{
//	  "query":      string,                       // required
//	  "scope":      "single"|"all",               // default "all"
//	  "dataset_id": string,                       // required when scope=single
//	  "mode":       "bm25"|"vector"|"hybrid",     // default "bm25"
//	  "layer":      "L0"|"L1"|"L2",               // default "L2"
//	  "top_k":      integer,                      // default 5
//	  "threshold":  number                        // default 0
//	}
//
// Output: JSON-encoded []knowledge.Hit.
//
// This is the v0.3.0 location of the helper that lives at
// [knowledge.NewSearchServiceTool] in v0.2.x. Behaviour is
// identical; only the import path changes.
func NewSearchServiceTool(svc *knowledge.Service) tool.Tool {
	return tool.FuncTool(
		tool.DefineSchema(
			"knowledge_search",
			"Search the knowledge base. Supports BM25 / vector / hybrid modes "+
				"and three layers (L0 abstract, L1 overview, L2 detail). "+
				"Use specific keywords for best results.",
			tool.Property("query", "string", "Search query"),
			tool.Property("scope", "string", `"single" or "all" (default "all")`),
			tool.Property("dataset_id", "string", "Required when scope=single"),
			tool.Property("mode", "string", `"bm25" | "vector" | "hybrid" (default "bm25")`),
			tool.Property("layer", "string", `"L0" | "L1" | "L2" (default "L2")`),
			tool.Property("top_k", "integer", "Number of results (default 5)"),
			tool.Property("threshold", "number", "Minimum fused score (default 0)"),
		).Required("query").Build(),
		func(ctx context.Context, args string) (string, error) {
			if svc == nil {
				return "", errdefs.NotAvailablef("knowledge service not available")
			}
			var p struct {
				Query     string  `json:"query"`
				Scope     string  `json:"scope"`
				DatasetID string  `json:"dataset_id"`
				Mode      string  `json:"mode"`
				Layer     string  `json:"layer"`
				TopK      int     `json:"top_k"`
				Threshold float64 `json:"threshold"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			q := knowledge.Query{
				Text:      p.Query,
				DatasetID: p.DatasetID,
				Mode:      knowledge.Mode(p.Mode),
				Layer:     knowledge.Layer(p.Layer),
				TopK:      p.TopK,
				Threshold: p.Threshold,
			}
			switch p.Scope {
			case "single":
				q.Scope = knowledge.ScopeSingleDataset
			case "all", "":
				q.Scope = knowledge.ScopeAllDatasets
			default:
				return "", errdefs.Validationf("knowledge: invalid scope %q", p.Scope)
			}
			res, err := svc.Search(ctx, q)
			if err != nil {
				return "", err
			}
			hits := []knowledge.Hit{}
			if res != nil {
				hits = res.Hits
			}
			data, err := json.Marshal(hits)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	)
}

// NewPutServiceTool exposes [knowledge.Service.PutDocument] to LLMs.
//
// Tool name:  "knowledge_put"
// Input JSON:
//
//	{
//	  "dataset_id": string,   // default "default"
//	  "name":       string,   // required
//	  "content":    string    // required
//	}
//
// Output:
//
//	{"status": "ok", "dataset_id": ..., "name": ..., "version": uint}
//
// This is the v0.3.0 location of the helper that lives at
// [knowledge.NewPutServiceTool] in v0.2.x. Behaviour is identical;
// only the import path changes.
func NewPutServiceTool(svc *knowledge.Service) tool.Tool {
	return tool.FuncTool(
		tool.DefineSchema(
			"knowledge_put",
			"Persist a document into the knowledge base. Use this for "+
				"durable knowledge (troubleshooting conclusions, design notes); "+
				"avoid temporary scratchpads.",
			tool.Property("dataset_id", "string", `Target dataset (default "default")`),
			tool.Property("name", "string", "Document name (include extension, e.g. .md)"),
			tool.Property("content", "string", "Document body"),
		).Required("name", "content").Build(),
		func(ctx context.Context, args string) (string, error) {
			if svc == nil {
				return "", errdefs.NotAvailablef("knowledge service not available")
			}
			var p struct {
				DatasetID string `json:"dataset_id"`
				Name      string `json:"name"`
				Content   string `json:"content"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if p.DatasetID == "" {
				p.DatasetID = defaultDatasetID
			}
			if err := svc.PutDocument(ctx, p.DatasetID, p.Name, p.Content); err != nil {
				return "", err
			}
			doc, err := svc.GetDocument(ctx, p.DatasetID, p.Name)
			if err != nil {
				return "", err
			}
			version := uint64(0)
			if doc != nil {
				version = doc.Version
			}
			resp, _ := json.Marshal(map[string]any{
				"status":     "ok",
				"dataset_id": p.DatasetID,
				"name":       p.Name,
				"version":    version,
			})
			return string(resp), nil
		},
	)
}
