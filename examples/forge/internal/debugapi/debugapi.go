// Package debugapi serves the demo's debug HTTP endpoints over an
// assembled app.
package debugapi

import (
	"encoding/json"
	"net/http"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
)

// NewHandler returns the debug HTTP handler for a workspace app.
func NewHandler(a *app.App) http.Handler {
	mux := http.NewServeMux()
	info := a.Info()
	mux.HandleFunc("GET /debug", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"agent":          info.AgentName,
			"context":        info.ContextID,
			"generate_model": info.GenerateModel,
			"memory_enabled": a.Memory() != nil,
		})
	})
	mux.HandleFunc("GET /debug/workspace", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"workspace": a.Describe()})
	})
	mux.HandleFunc("GET /debug/memory", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"enabled": a.Memory() != nil,
			"scope":   info.MemoryScope,
			"top_k":   info.MemoryTopK,
		})
	})
	mux.HandleFunc("GET /debug/history", func(w http.ResponseWriter, r *http.Request) {
		contextID := r.URL.Query().Get("context_id")
		if contextID == "" {
			contextID = info.ContextID
		}
		writeJSON(w, map[string]any{
			"context_id": contextID,
			"message":    "conversation transcript is committed to memory per turn; use /debug/recall",
		})
	})
	mux.HandleFunc("GET /debug/recall", func(w http.ResponseWriter, r *http.Request) {
		if a.Memory() == nil {
			writeJSON(w, map[string]any{"enabled": false})
			return
		}
		query := r.URL.Query().Get("query")
		if query == "" {
			query = r.URL.Query().Get("text")
		}
		if query == "" {
			http.Error(w, "recall requires ?query=", http.StatusBadRequest)
			return
		}
		result, err := a.Memory().Context(r.Context(), sdkmemory.ContextRequest{
			Scope: sdkmemory.Scope{
				RuntimeID: info.MemoryScope.RuntimeID,
				UserID:    info.MemoryScope.UserID,
				AgentID:   info.MemoryScope.AgentID,
			},
			Query:  query,
			Budget: sdkmemory.Budget{MaxItems: info.MemoryTopK},
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hits := make([]map[string]any, 0, len(result.Items))
		for _, item := range result.Items {
			hits = append(hits, map[string]any{"content": item.Content.Text(), "score": item.Score})
		}
		writeJSON(w, map[string]any{"enabled": true, "query": query, "hits": hits})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
