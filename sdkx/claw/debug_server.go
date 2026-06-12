package claw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// NewDebugHTTPHandler returns the debug API handler for app.
func NewDebugHTTPHandler(app *Claw) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.ServeDebugHTTP(w, r)
	})
}

// ServeDebugHTTP serves Claw debug endpoints.
func (c *Claw) ServeDebugHTTP(w http.ResponseWriter, r *http.Request) {
	if c == nil {
		writeDebugError(w, http.StatusInternalServerError, "claw: nil app")
		return
	}
	switch r.URL.Path {
	case "/debug", "/debug/":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		writeDebugJSON(w, http.StatusOK, map[string]any{
			"endpoints": []string{
				"/debug/workspace",
				"/debug/history",
				"/debug/memory",
				"/debug/recall",
			},
		})
	case "/debug/workspace":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		c.serveDebugWorkspace(w)
	case "/debug/history":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		c.serveDebugHistory(w, r)
	case "/debug/memory":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		c.serveDebugMemory(w)
	case "/debug/recall":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		c.serveDebugRecall(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (c *Claw) serveDebugWorkspace(w http.ResponseWriter) {
	cfg := c.cfg
	writeDebugJSON(w, http.StatusOK, debugWorkspaceResponse{
		Workspace: debugWorkspaceInfo{
			Root:        workspaceRoot(c.ws),
			MemoryRoot:  cfg.Workspace.MemoryRoot,
			StateRoot:   cfg.Workspace.StateRoot,
			HistoryRoot: cfg.Workspace.HistoryRoot,
		},
		Conversation: cfg.Conversation,
		Agent: debugAgentInfo{
			ID:          cfg.Agent.ID,
			Name:        cfg.Agent.Name,
			Description: cfg.Agent.Description,
		},
		Models: debugModelsInfo{
			Chat:       cfg.Models.Chat,
			Extractor:  cfg.Models.Extractor,
			Embedder:   cfg.Models.Embedder,
			LLM:        cfg.Models.LLM,
			Embedding:  cfg.Models.Embedding,
			Embeddings: cfg.Models.Embeddings,
		},
		History: debugHistoryInfo{
			Enabled: cfg.History.Enabled,
			Kind:    cfg.History.Kind,
		},
		Memory: debugMemoryEnabledInfo{
			Enabled: cfg.Memory.Enabled,
			Backend: cfg.Memory.Retrieval.Backend,
		},
	})
}

func (c *Claw) serveDebugHistory(w http.ResponseWriter, r *http.Request) {
	contextID := strings.TrimSpace(r.URL.Query().Get("context_id"))
	if contextID == "" {
		contextID = c.cfg.Conversation.ContextID
	}
	if contextID == "" {
		contextID = defaultConversationContextID
	}
	resp := debugHistoryResponse{
		Enabled:   c.history != nil,
		ContextID: contextID,
	}
	if c.history == nil {
		writeDebugJSON(w, http.StatusOK, resp)
		return
	}
	messages, err := c.history.load(r.Context(), contextID)
	if err != nil {
		writeDebugError(w, http.StatusInternalServerError, fmt.Sprintf("load history: %v", err))
		return
	}
	resp.Messages = messages
	resp.Count = len(messages)
	writeDebugJSON(w, http.StatusOK, resp)
}

func (c *Claw) serveDebugMemory(w http.ResponseWriter) {
	cfg := c.cfg.Memory
	if c.memory != nil {
		cfg = c.memory.cfg
	}
	writeDebugJSON(w, http.StatusOK, debugMemoryResponse{
		Enabled: c.memory != nil,
		Root:    c.cfg.Workspace.MemoryRoot,
		Scope:   cfg.Scope,
		Write: debugMemoryWriteInfo{
			SaveConversation: cfg.Write.SaveConversation,
			Mode:             cfg.Write.Mode,
			Tier:             cfg.Write.Tier,
			BoardFacts:       cfg.Write.BoardFacts,
		},
		Recall: debugMemoryRecallInfo{
			Enabled:      cfg.Recall.Enabled,
			TopK:         cfg.Recall.TopK,
			Inject:       cfg.Recall.Inject,
			BoardVar:     cfg.Recall.BoardVar,
			ProfileNames: sortedRecallProfileNames(cfg.Recall.Profiles),
		},
		Retrieval: cfg.Retrieval,
		Layout:    cfg.Layout,
	})
}

func (c *Claw) serveDebugRecall(w http.ResponseWriter, r *http.Request) {
	if c.memory == nil || c.memory.mem == nil {
		writeDebugJSON(w, http.StatusOK, debugRecallResponse{
			Enabled: false,
			Hits:    []debugRecallHit{},
		})
		return
	}
	var req debugRecallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDebugError(w, http.StatusBadRequest, fmt.Sprintf("decode recall request: %v", err))
		return
	}
	query := c.memory.debugRecallQuery(req)
	if recallQueryEmpty(query) {
		writeDebugError(w, http.StatusBadRequest, "recall request requires text or structured query fields")
		return
	}
	hits, err := c.memory.mem.Recall(r.Context(), c.memory.scope, query)
	if err != nil {
		writeDebugError(w, http.StatusInternalServerError, fmt.Sprintf("recall: %v", err))
		return
	}
	hits = filterRecallHits(hits, req.Lanes)
	out := make([]debugRecallHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, debugRecallHit{
			ID:        hit.Fact.ID,
			Kind:      string(hit.Fact.Kind),
			Content:   hit.Fact.Content,
			Subject:   hit.Fact.Subject,
			Predicate: hit.Fact.Predicate,
			Object:    hit.Fact.Object,
			Entities:  hit.Fact.Entities,
			Score:     hit.Score,
			Sources:   hit.Sources,
		})
	}
	writeDebugJSON(w, http.StatusOK, debugRecallResponse{
		Enabled: true,
		Query:   req,
		Hits:    out,
		Count:   len(out),
	})
}

func (m *memoryRuntime) debugRecallQuery(req debugRecallRequest) recall.Query {
	limit := req.TopK
	if limit <= 0 {
		limit = m.cfg.Recall.TopK
	}
	if limit <= 0 {
		limit = 5
	}
	return recall.Query{
		Text:           strings.TrimSpace(req.Text),
		Entities:       nonEmptyStrings(req.Entities),
		Limit:          limit,
		Subject:        strings.TrimSpace(req.Subject),
		Predicate:      strings.TrimSpace(req.Predicate),
		Object:         strings.TrimSpace(req.Object),
		Kinds:          recallKinds(req.Kinds),
		GraphHops:      req.GraphHops,
		IncludeRetired: req.IncludeRetired,
	}
}

type debugWorkspaceResponse struct {
	Workspace    debugWorkspaceInfo     `json:"workspace"`
	Conversation ConversationConfig     `json:"conversation"`
	Agent        debugAgentInfo         `json:"agent"`
	Models       debugModelsInfo        `json:"models"`
	History      debugHistoryInfo       `json:"history"`
	Memory       debugMemoryEnabledInfo `json:"memory"`
}

type debugWorkspaceInfo struct {
	Root        string `json:"root,omitempty"`
	MemoryRoot  string `json:"memory_root"`
	StateRoot   string `json:"state_root"`
	HistoryRoot string `json:"history_root"`
}

type debugAgentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type debugModelsInfo struct {
	Chat       string                 `json:"chat,omitempty"`
	Extractor  string                 `json:"extractor,omitempty"`
	Embedder   string                 `json:"embedder,omitempty"`
	LLM        map[string]ModelConfig `json:"llm,omitempty"`
	Embedding  map[string]ModelConfig `json:"embedding,omitempty"`
	Embeddings map[string]ModelConfig `json:"embeddings,omitempty"`
}

type debugHistoryInfo struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind,omitempty"`
}

type debugMemoryEnabledInfo struct {
	Enabled bool   `json:"enabled"`
	Backend string `json:"backend,omitempty"`
}

type debugHistoryResponse struct {
	Enabled   bool            `json:"enabled"`
	ContextID string          `json:"context_id"`
	Count     int             `json:"count"`
	Messages  []model.Message `json:"messages,omitempty"`
}

type debugMemoryResponse struct {
	Enabled   bool                  `json:"enabled"`
	Root      string                `json:"root"`
	Scope     MemoryScopeConfig     `json:"scope"`
	Write     debugMemoryWriteInfo  `json:"write"`
	Recall    debugMemoryRecallInfo `json:"recall"`
	Retrieval MemoryRetrievalConfig `json:"retrieval"`
	Layout    MemoryLayoutConfig    `json:"layout"`
}

type debugMemoryWriteInfo struct {
	SaveConversation bool                         `json:"save_conversation"`
	Mode             string                       `json:"mode,omitempty"`
	Tier             string                       `json:"tier,omitempty"`
	BoardFacts       []MemoryWriteBoardFactConfig `json:"board_facts,omitempty"`
}

type debugMemoryRecallInfo struct {
	Enabled      bool     `json:"enabled"`
	TopK         int      `json:"top_k"`
	Inject       string   `json:"inject,omitempty"`
	BoardVar     string   `json:"board_var,omitempty"`
	ProfileNames []string `json:"profiles,omitempty"`
}

type debugRecallRequest struct {
	Text           string   `json:"text,omitempty"`
	TopK           int      `json:"top_k,omitempty"`
	Lanes          []string `json:"lanes,omitempty"`
	Entities       []string `json:"entities,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	Predicate      string   `json:"predicate,omitempty"`
	Object         string   `json:"object,omitempty"`
	Kinds          []string `json:"kinds,omitempty"`
	GraphHops      int      `json:"graph_hops,omitempty"`
	IncludeRetired bool     `json:"include_retired,omitempty"`
}

type debugRecallResponse struct {
	Enabled bool               `json:"enabled"`
	Query   debugRecallRequest `json:"query,omitempty"`
	Count   int                `json:"count"`
	Hits    []debugRecallHit   `json:"hits"`
}

type debugRecallHit struct {
	ID        string   `json:"id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Content   string   `json:"content"`
	Subject   string   `json:"subject,omitempty"`
	Predicate string   `json:"predicate,omitempty"`
	Object    string   `json:"object,omitempty"`
	Entities  []string `json:"entities,omitempty"`
	Score     float64  `json:"score,omitempty"`
	Sources   []string `json:"sources,omitempty"`
}

type workspaceRooter interface {
	Root() string
}

func workspaceRoot(ws any) string {
	if rooted, ok := ws.(workspaceRooter); ok {
		return rooted.Root()
	}
	return ""
}

func sortedRecallProfileNames(profiles map[string]MemoryRecallProfileConfig) []string {
	if len(profiles) == 0 {
		return nil
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func writeDebugJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeDebugError(w http.ResponseWriter, status int, message string) {
	writeDebugJSON(w, status, map[string]any{
		"error": message,
	})
}

func writeMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
