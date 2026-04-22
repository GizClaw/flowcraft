package knowledge

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

const defaultDatasetID = "default"

func NewSearchTool(ks Store) tool.Tool {
	return tool.FuncTool(
		tool.DefineSchema("knowledge_search",
			"Search the knowledge base using keyword matching. "+
				"Use specific, concrete keywords (e.g. node type names, error messages) "+
				"rather than abstract queries for best results. "+
				"Automatically searches across all datasets and returns ranked results.",
			tool.Property("query", "string", "Search query"),
			tool.Property("top_k", "integer", "Number of results to return (default 5)"),
		).Required("query").Build(),
		func(ctx context.Context, args string) (string, error) {
			if ks == nil {
				return "", errdefs.NotAvailablef("knowledge store not available")
			}
			var p struct {
				Query string `json:"query"`
				TopK  int    `json:"top_k"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return "", err
			}
			if p.TopK <= 0 {
				p.TopK = 5
			}
			results, err := ks.Search(ctx, "", p.Query, SearchOptions{TopK: p.TopK})
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(results)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	)
}

func NewAddTool(ks Store) tool.Tool {
	return tool.FuncTool(
		tool.DefineSchema("knowledge_add",
			"Add a document to the knowledge base. "+
				"Use this to persist reusable knowledge such as troubleshooting conclusions, "+
				"best practices, or design decisions that may benefit future conversations. "+
				"Do NOT use this for personal preferences or temporary notes.",
			tool.Property("dataset_id", "string", "Target dataset ID (default: \"default\")"),
			tool.Property("name", "string", "Document name (include .md suffix)"),
			tool.Property("content", "string", "Document content in markdown"),
		).Required("name", "content").Build(),
		func(ctx context.Context, args string) (string, error) {
			if ks == nil {
				return "", errdefs.NotAvailablef("knowledge store not available")
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
			if err := ks.AddDocument(ctx, p.DatasetID, p.Name, p.Content); err != nil {
				return "", err
			}
			resp, _ := json.Marshal(map[string]string{
				"status":     "ok",
				"dataset_id": p.DatasetID,
				"name":       p.Name,
			})
			return string(resp), nil
		},
	)
}
