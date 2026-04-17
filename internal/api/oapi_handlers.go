package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	"github.com/rs/xid"
	"gopkg.in/yaml.v3"
)

// configuredModelKeyPrefix is the store key prefix for per-model provider rows.
const configuredModelKeyPrefix = "model:"

type oapiHandler struct {
	s *Server
}

func newOAPIHandler(s *Server) *oapiHandler {
	return &oapiHandler{s: s}
}

func (h *oapiHandler) NewError(ctx context.Context, err error) *oas.ErrorStatusCode {
	return newErrorResponse(err)
}

// toOAS converts src to *T via JSON round-trip.
// Both model types and ogen types carry compatible json tags.
func toOAS[T any](src any) (*T, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	var dst T
	if err := json.Unmarshal(data, &dst); err != nil {
		return nil, err
	}
	return &dst, nil
}

func toOASSlice[T any](src any) ([]T, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	var dst []T
	if err := json.Unmarshal(data, &dst); err != nil {
		return nil, err
	}
	return dst, nil
}

// ══════════════════════════ Health ══════════════════════════

func (h *oapiHandler) HealthCheck(ctx context.Context) (*oas.HealthStatus, error) {
	return &oas.HealthStatus{Status: "ok"}, nil
}

// ══════════════════════════ Auth ══════════════════════════

func (h *oapiHandler) Login(ctx context.Context, req *oas.LoginRequest) (*oas.LoginResponse, error) {
	if h.s.jwt == nil {
		return &oas.LoginResponse{
			Authenticated: oas.NewOptBool(true),
			AuthEnabled:   oas.NewOptBool(false),
			Principal:     oas.NewOptString("owner"),
			AuthMode:      oas.NewOptString("none"),
		}, nil
	}
	cred, err := h.s.deps.Platform.Store.GetOwnerCredential(ctx)
	if err != nil {
		return nil, errdefs.Unauthorizedf("invalid credentials")
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.APIKey)) != nil {
		return nil, errdefs.Unauthorizedf("invalid credentials")
	}
	w, wOK := HTTPResponseWriterFromContext(ctx)
	rr, rOK := HTTPRequestFromContext(ctx)
	token, expiresAt, err := h.s.jwt.Issue(cred.Username)
	if err != nil {
		return nil, errdefs.Internalf("failed to issue token")
	}
	if wOK && rOK && rr != nil {
		h.s.setAuthCookie(w, token, expiresAt, rr.TLS != nil)
	}
	return &oas.LoginResponse{
		Authenticated: oas.NewOptBool(true),
		AuthEnabled:   oas.NewOptBool(true),
		Principal:     oas.NewOptString(cred.Username),
		AuthMode:      oas.NewOptString("jwt"),
	}, nil
}

func (h *oapiHandler) Logout(ctx context.Context) error {
	w, wOK := HTTPResponseWriterFromContext(ctx)
	rr, rOK := HTTPRequestFromContext(ctx)
	if wOK && rOK && rr != nil {
		h.s.clearAuthCookie(w, rr.TLS != nil)
	}
	return nil
}

func (h *oapiHandler) GetSession(ctx context.Context) (*oas.SessionInfo, error) {
	rr, ok := HTTPRequestFromContext(ctx)
	if !ok || rr == nil {
		return &oas.SessionInfo{
			Authenticated: oas.NewOptBool(false),
			AuthEnabled:   oas.NewOptBool(h.s.jwt != nil),
		}, nil
	}
	claims, authed := h.s.authenticateRequest(rr)
	if !authed || claims == nil {
		return &oas.SessionInfo{
			Authenticated: oas.NewOptBool(false),
			AuthEnabled:   oas.NewOptBool(h.s.jwt != nil),
		}, nil
	}
	return &oas.SessionInfo{
		Authenticated: oas.NewOptBool(true),
		AuthEnabled:   oas.NewOptBool(h.s.jwt != nil),
		Principal:     oas.NewOptString(claims.Username),
		AuthMode:      oas.NewOptString("jwt"),
	}, nil
}

func (h *oapiHandler) GetAuthConfig(ctx context.Context) (*oas.AuthConfig, error) {
	return &oas.AuthConfig{
		AuthEnabled: oas.NewOptBool(h.s.jwt != nil),
		LoginMode:   oas.NewOptString("session"),
	}, nil
}

// ══════════════════════════ Agents ══════════════════════════

func (h *oapiHandler) ListAgents(ctx context.Context, params oas.ListAgentsParams) (*oas.AgentList, error) {
	opts := model.ListOptions{}
	if v, ok := params.Limit.Get(); ok {
		opts.Limit = v
	}
	if v, ok := params.Cursor.Get(); ok {
		opts.Cursor = v
	}
	agents, page, err := h.s.deps.Platform.Store.ListAgents(ctx, opts)
	if err != nil {
		return nil, err
	}
	oasAgents, err := toOASSlice[oas.Agent](agents)
	if err != nil {
		return nil, err
	}
	result := &oas.AgentList{Data: oasAgents}
	if page != nil {
		result.HasMore = oas.NewOptBool(page.HasMore)
		if page.NextCursor != "" {
			result.NextCursor = oas.NewOptString(page.NextCursor)
		}
	}
	return result, nil
}

func (h *oapiHandler) CreateAgent(ctx context.Context, req *oas.CreateAgentRequest) (*oas.Agent, error) {
	if req.Name == "" {
		return nil, errdefs.Validationf("name is required")
	}
	agent := &model.Agent{
		AgentID:     xid.New().String(),
		Name:        req.Name,
		Type:        model.AgentType(req.Type.Or("")),
		Description: req.Description.Or(""),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if agent.Type == "" {
		agent.Type = model.AgentTypeWorkflow
	}

	if req.Config != nil {
		raw, _ := json.Marshal(req.Config)
		var cfg model.AgentConfig
		_ = json.Unmarshal(raw, &cfg)
		agent.Config = cfg
	}
	if req.InputSchema != nil {
		raw, _ := json.Marshal(req.InputSchema)
		_ = json.Unmarshal(raw, &agent.InputSchema)
	}

	if tmplName, ok := req.Template.Get(); ok && tmplName != "" {
		graphMap, err := h.s.deps.Platform.InstantiateTemplate(tmplName, nil)
		if err != nil {
			return nil, errdefs.Validationf("template instantiation failed: %v", err)
		}
		if graphMap == nil {
			return nil, errdefs.NotFoundf("template %s not found", tmplName)
		}
		raw, _ := json.Marshal(graphMap)
		var gd model.GraphDefinition
		if err := json.Unmarshal(raw, &gd); err != nil {
			return nil, errdefs.Validationf("template graph parse failed: %v", err)
		}
		agent.StrategyDef = model.NewGraphStrategy(&gd)
	} else if req.GraphDefinition != nil {
		raw, _ := json.Marshal(req.GraphDefinition)
		var gd model.GraphDefinition
		_ = json.Unmarshal(raw, &gd)
		agent.StrategyDef = model.NewGraphStrategy(&gd)
	}

	created, err := h.s.deps.Platform.Store.CreateAgent(ctx, agent)
	if err != nil {
		return nil, err
	}
	h.saveDraftVersion(ctx, created)
	h.publishAgentConfigChanged(ctx, created.AgentID)
	return toOAS[oas.Agent](created)
}

func (h *oapiHandler) GetAgent(ctx context.Context, params oas.GetAgentParams) (*oas.Agent, error) {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.Agent](agent)
}

func (h *oapiHandler) UpdateAgent(ctx context.Context, req *oas.UpdateAgentRequest, params oas.UpdateAgentParams) (*oas.Agent, error) {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	if agent.Type == model.AgentTypeCoPilot {
		return nil, errdefs.Forbiddenf("copilot agent cannot be modified directly")
	}
	if v, ok := req.Name.Get(); ok {
		agent.Name = v
	}
	if v, ok := req.Description.Get(); ok {
		agent.Description = v
	}
	if req.Config != nil {
		raw, _ := json.Marshal(req.Config)
		var cfg model.AgentConfig
		_ = json.Unmarshal(raw, &cfg)
		agent.Config = cfg
	}
	if req.GraphDefinition != nil {
		raw, _ := json.Marshal(req.GraphDefinition)
		var gd model.GraphDefinition
		_ = json.Unmarshal(raw, &gd)
		agent.StrategyDef = model.NewGraphStrategy(&gd)
	}
	if req.InputSchema != nil {
		raw, _ := json.Marshal(req.InputSchema)
		_ = json.Unmarshal(raw, &agent.InputSchema)
	}
	agent.UpdatedAt = time.Now()

	updated, err := h.s.deps.Platform.Store.UpdateAgent(ctx, agent)
	if err != nil {
		return nil, err
	}
	h.saveDraftVersion(ctx, updated)
	h.publishAgentConfigChanged(ctx, updated.AgentID)
	return toOAS[oas.Agent](updated)
}

func (h *oapiHandler) DeleteAgent(ctx context.Context, params oas.DeleteAgentParams) error {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return err
	}
	if agent.Type == model.AgentTypeCoPilot {
		return errdefs.Forbiddenf("copilot agent cannot be deleted")
	}
	return h.s.deps.Platform.Store.DeleteAgent(ctx, params.ID)
}

func (h *oapiHandler) AbortActor(ctx context.Context, params oas.AbortActorParams) (*oas.AbortActorOK, error) {
	aborted, err := h.s.deps.Platform.AbortAgent(ctx, params.AgentID)
	if err != nil {
		return nil, err
	}
	if !aborted {
		return nil, errdefs.NotFoundf("actor %s not found or not running", params.AgentID)
	}
	return &oas.AbortActorOK{Aborted: oas.NewOptBool(true)}, nil
}

// ══════════════════════════ Conversations ══════════════════════════

func (h *oapiHandler) ListConversations(ctx context.Context, params oas.ListConversationsParams) (*oas.ConversationList, error) {
	agentID := params.AgentID.Or("")
	opts := model.ListOptions{
		Limit:  params.Limit.Or(0),
		Cursor: params.Cursor.Or(""),
	}
	convs, page, err := h.s.deps.Platform.Store.ListConversations(ctx, agentID, opts)
	if err != nil {
		return nil, err
	}
	oasConvs, err := toOASSlice[oas.Conversation](convs)
	if err != nil {
		return nil, err
	}
	result := &oas.ConversationList{Data: oasConvs}
	if page != nil {
		result.HasMore = oas.NewOptBool(page.HasMore)
		if page.NextCursor != "" {
			result.NextCursor = oas.NewOptString(page.NextCursor)
		}
	}
	return result, nil
}

func (h *oapiHandler) GetMessages(ctx context.Context, params oas.GetMessagesParams) (*oas.MessageList, error) {
	msgs, err := h.s.deps.Platform.Store.GetMessages(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	oasMsgs, err := toOASSlice[oas.Message](msgs)
	if err != nil {
		return nil, err
	}
	return &oas.MessageList{Data: oasMsgs}, nil
}

// ══════════════════════════ Workflow Runs ══════════════════════════

func (h *oapiHandler) ListWorkflowRuns(ctx context.Context, params oas.ListWorkflowRunsParams) (*oas.WorkflowRunList, error) {
	agentID := params.AgentID.Or("")
	opts := model.ListOptions{
		Limit:  params.Limit.Or(0),
		Cursor: params.Cursor.Or(""),
	}
	runs, page, err := h.s.deps.Platform.Store.ListWorkflowRuns(ctx, agentID, opts)
	if err != nil {
		return nil, err
	}
	oasRuns, err := toOASSlice[oas.WorkflowRun](runs)
	if err != nil {
		return nil, err
	}
	result := &oas.WorkflowRunList{Data: oasRuns}
	if page != nil {
		result.HasMore = oas.NewOptBool(page.HasMore)
		if page.NextCursor != "" {
			result.NextCursor = oas.NewOptString(page.NextCursor)
		}
	}
	return result, nil
}

func (h *oapiHandler) GetWorkflowRun(ctx context.Context, params oas.GetWorkflowRunParams) (*oas.WorkflowRun, error) {
	run, err := h.s.deps.Platform.Store.GetWorkflowRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.WorkflowRun](run)
}

func (h *oapiHandler) GetWorkflowRunEvents(ctx context.Context, params oas.GetWorkflowRunEventsParams) (*oas.ExecutionEventList, error) {
	events, err := h.s.deps.Platform.Store.ListExecutionEvents(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	oasEvents, err := toOASSlice[oas.ExecutionEvent](events)
	if err != nil {
		return nil, err
	}
	return &oas.ExecutionEventList{Data: oasEvents}, nil
}

func (h *oapiHandler) GetRunStatus(ctx context.Context, params oas.GetRunStatusParams) (*oas.WorkflowRun, error) {
	run, err := h.s.deps.Platform.Store.GetWorkflowRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.WorkflowRun](run)
}

// ══════════════════════════ Knowledge ══════════════════════════

func (h *oapiHandler) ListDatasets(ctx context.Context) (*oas.DatasetList, error) {
	datasets, err := h.s.deps.Platform.Store.ListDatasets(ctx)
	if err != nil {
		return nil, err
	}
	oasDS, err := toOASSlice[oas.Dataset](datasets)
	if err != nil {
		return nil, err
	}
	return &oas.DatasetList{Data: oasDS}, nil
}

func (h *oapiHandler) CreateDataset(ctx context.Context, req *oas.CreateDatasetRequest) (*oas.Dataset, error) {
	if req.Name == "" {
		return nil, errdefs.Validationf("name is required")
	}
	ds := &model.Dataset{
		Name:        req.Name,
		Description: req.Description.Or(""),
		AgentID:     req.AgentID.Or(""),
	}
	created, err := h.s.deps.Platform.Store.CreateDataset(ctx, ds)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.Dataset](created)
}

func (h *oapiHandler) GetDataset(ctx context.Context, params oas.GetDatasetParams) (*oas.Dataset, error) {
	ds, err := h.s.deps.Platform.Store.GetDataset(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.Dataset](ds)
}

func (h *oapiHandler) DeleteDataset(ctx context.Context, params oas.DeleteDatasetParams) error {
	return h.s.deps.Platform.Store.DeleteDataset(ctx, params.ID)
}

func (h *oapiHandler) ListDocuments(ctx context.Context, params oas.ListDocumentsParams) (*oas.DocumentList, error) {
	docs, err := h.s.deps.Platform.Store.ListDocuments(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	oasDocs, err := toOASSlice[oas.DatasetDocument](docs)
	if err != nil {
		return nil, err
	}
	return &oas.DocumentList{Data: oasDocs}, nil
}

func (h *oapiHandler) AddDocument(ctx context.Context, req *oas.AddDocumentRequest, params oas.AddDocumentParams) (*oas.DatasetDocument, error) {
	if req.Name == "" || req.Content == "" {
		return nil, errdefs.Validationf("name and content are required")
	}
	doc, err := h.s.deps.Platform.Store.AddDocument(ctx, params.ID, req.Name, req.Content)
	if err != nil {
		return nil, err
	}
	if h.s.deps.Platform.Knowledge != nil {
		if addErr := h.s.deps.Platform.Knowledge.AddDocument(ctx, params.ID, req.Name, req.Content); addErr != nil {
			_ = h.s.deps.Platform.Store.DeleteDocument(ctx, params.ID, doc.ID)
			return nil, addErr
		}
	}
	return toOAS[oas.DatasetDocument](doc)
}

func (h *oapiHandler) DeleteDocument(ctx context.Context, params oas.DeleteDocumentParams) error {
	doc, err := h.s.deps.Platform.Store.GetDocument(ctx, params.ID, params.DocId)
	if err != nil {
		return err
	}
	if err := h.s.deps.Platform.Store.DeleteDocument(ctx, params.ID, params.DocId); err != nil {
		return err
	}
	if h.s.deps.Platform.Knowledge != nil && doc != nil {
		_ = h.s.deps.Platform.Knowledge.DeleteDocument(ctx, params.ID, doc.Name)
	}
	return nil
}

func (h *oapiHandler) QueryDocuments(ctx context.Context, req *oas.DatasetQueryRequest, params oas.QueryDocumentsParams) (*oas.QueryResultList, error) {
	if req.Query == "" {
		return nil, errdefs.Validationf("query is required")
	}
	if h.s.deps.Platform.Knowledge == nil {
		return nil, errdefs.Internalf("knowledge store not configured")
	}
	opts := knowledge.SearchOptions{}
	if v, ok := req.TopK.Get(); ok {
		opts.TopK = v
	}
	if v, ok := req.Threshold.Get(); ok {
		opts.Threshold = v
	}
	if v, ok := req.MaxLayer.Get(); ok {
		layerMap := map[int]knowledge.ContextLayer{
			0: knowledge.LayerAbstract,
			1: knowledge.LayerOverview,
			2: knowledge.LayerDetail,
		}
		if ml, found := layerMap[v]; found {
			opts.MaxLayer = ml
		}
	}
	results, err := h.s.deps.Platform.Knowledge.Search(ctx, params.ID, req.Query, opts)
	if err != nil {
		return nil, err
	}
	oasResults := make([]oas.QueryResult, len(results))
	for i, res := range results {
		oasResults[i] = oas.QueryResult{
			Content:      oas.NewOptString(res.Content),
			Score:        oas.NewOptFloat64(res.Score),
			DocumentName: oas.NewOptString(res.DocName),
			ChunkIndex:   oas.NewOptInt(res.ChunkIndex),
		}
	}
	return &oas.QueryResultList{Data: oasResults}, nil
}

// ══════════════════════════ Graph Versions ══════════════════════════

func (h *oapiHandler) ListVersions(ctx context.Context, params oas.ListVersionsParams) (*oas.GraphVersionList, error) {
	if h.s.deps.Platform.VersionStore == nil {
		return nil, errDepMissing("version store")
	}
	versions, err := h.s.deps.Platform.VersionStore.ListVersions(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	oasVers, err := toOASSlice[oas.GraphVersion](versions)
	if err != nil {
		return nil, err
	}
	return &oas.GraphVersionList{Data: oasVers}, nil
}

func (h *oapiHandler) PublishVersion(ctx context.Context, req *oas.PublishVersionRequest, params oas.PublishVersionParams) (*oas.GraphVersion, error) {
	if h.s.deps.Platform.VersionStore == nil {
		return nil, errDepMissing("version store")
	}
	desc := req.Description.Or("")
	gv, err := h.s.deps.Platform.VersionStore.Publish(ctx, params.ID, 0, desc)
	if err != nil {
		return nil, err
	}
	agent, getErr := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if getErr == nil && gv.GraphDef != nil {
		agent.StrategyDef = model.NewGraphStrategy(gv.GraphDef)
		_, _ = h.s.deps.Platform.Store.UpdateAgent(ctx, agent)
		h.publishAgentConfigChanged(ctx, params.ID)
	}
	return toOAS[oas.GraphVersion](gv)
}

func (h *oapiHandler) RollbackVersion(ctx context.Context, params oas.RollbackVersionParams) (*oas.GraphVersion, error) {
	if h.s.deps.Platform.VersionStore == nil {
		return nil, errDepMissing("version store")
	}
	gv, err := h.s.deps.Platform.VersionStore.Rollback(ctx, params.ID, params.Ver)
	if err != nil {
		return nil, err
	}
	agent, getErr := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if getErr == nil && gv.GraphDef != nil {
		agent.StrategyDef = model.NewGraphStrategy(gv.GraphDef)
		_, _ = h.s.deps.Platform.Store.UpdateAgent(ctx, agent)
		h.publishAgentConfigChanged(ctx, params.ID)
	}
	return toOAS[oas.GraphVersion](gv)
}

func (h *oapiHandler) DiffVersions(ctx context.Context, params oas.DiffVersionsParams) (*oas.VersionDiff, error) {
	if h.s.deps.Platform.VersionStore == nil {
		return nil, errDepMissing("version store")
	}
	if params.V1 <= 0 || params.V2 <= 0 {
		return nil, errdefs.Validationf("v1 and v2 must be positive integers")
	}
	if params.V1 == params.V2 {
		return nil, errdefs.Validationf("v1 and v2 must be different")
	}
	diff, err := h.s.deps.Platform.VersionStore.Diff(ctx, params.ID, params.V1, params.V2)
	if err != nil {
		return nil, fmt.Errorf("diff versions: %w", err)
	}
	return toOAS[oas.VersionDiff](diff)
}

// ══════════════════════════ Graph History ══════════════════════════

func (h *oapiHandler) GetGraphHistory(ctx context.Context, params oas.GetGraphHistoryParams) (*oas.GraphOperationList, error) {
	opts := model.ListOptions{
		Limit:  params.Limit.Or(0),
		Cursor: params.Cursor.Or(""),
	}
	ops, page, err := h.s.deps.Platform.Store.ListGraphOperations(ctx, params.ID, opts)
	if err != nil {
		return nil, err
	}
	oasOps, err := toOASSlice[oas.GraphOperation](ops)
	if err != nil {
		return nil, err
	}
	result := &oas.GraphOperationList{Data: oasOps}
	if page != nil {
		result.HasMore = oas.NewOptBool(page.HasMore)
		if page.NextCursor != "" {
			result.NextCursor = oas.NewOptString(page.NextCursor)
		}
	}
	return result, nil
}

// ══════════════════════════ Compile + DryRun ══════════════════════════

func (h *oapiHandler) CompileGraph(ctx context.Context, params oas.CompileGraphParams) (*oas.CompileResult, error) {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	gd := agent.StrategyDef.AsGraph()
	if gd == nil {
		return nil, errdefs.Validationf("agent has no graph definition")
	}
	compiled, compileErr := h.s.deps.Platform.Compiler.Compile(gd)
	result := &oas.CompileResult{}
	if compileErr != nil {
		result.Success = oas.NewOptBool(false)
		result.Errors = []oas.CompileIssue{{Message: oas.NewOptString(compileErr.Error())}}
	} else {
		result.Success = oas.NewOptBool(true)
		if len(compiled.Warnings) > 0 {
			warnings := make([]oas.CompileIssue, len(compiled.Warnings))
			for i, w := range compiled.Warnings {
				warnings[i] = oas.CompileIssue{
					Code:    oas.NewOptString(w.Code),
					Message: oas.NewOptString(w.Message),
					NodeIds: w.NodeIDs,
				}
			}
			result.Warnings = warnings
		}
	}
	return result, nil
}

func (h *oapiHandler) DryRun(ctx context.Context, params oas.DryRunParams) (*oas.DryRunResult, error) {
	if h.s.deps.Platform.Compiler == nil {
		return nil, errDepMissing("compiler")
	}
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	gd := agent.StrategyDef.AsGraph()
	if gd == nil {
		return &oas.DryRunResult{
			Valid: oas.NewOptBool(false),
			Warnings: []oas.CompileIssue{
				{Code: oas.NewOptString("no_graph"), Message: oas.NewOptString("no graph definition")},
			},
		}, nil
	}
	compiled, compileErr := h.s.deps.Platform.Compiler.Compile(gd)
	result := &oas.DryRunResult{}
	if compileErr != nil {
		result.Valid = oas.NewOptBool(false)
		result.Warnings = []oas.CompileIssue{
			{Code: oas.NewOptString("compile_error"), Message: oas.NewOptString(compileErr.Error())},
		}
		return result, nil
	}
	nodeWarnings := make(map[string][]string)
	for _, w := range compiled.Warnings {
		for _, nid := range w.NodeIDs {
			nodeWarnings[nid] = append(nodeWarnings[nid], w.Message)
		}
	}
	nodeResults := make([]oas.NodeValidationResult, 0, len(compiled.NodeDefs))
	for _, nd := range compiled.NodeDefs {
		nodeResults = append(nodeResults, oas.NodeValidationResult{
			NodeID:   oas.NewOptString(nd.ID),
			NodeType: oas.NewOptString(nd.Type),
			Valid:    oas.NewOptBool(true),
			Warnings: nodeWarnings[nd.ID],
		})
	}
	result.Valid = oas.NewOptBool(true)
	result.NodeResults = nodeResults
	result.Warnings = dryRunGroupWarningsByCode(compiled.Warnings)
	return result, nil
}

func dryRunGroupWarningsByCode(warnings []compiler.Warning) []oas.CompileIssue {
	groupMap := make(map[string]*oas.CompileIssue)
	var order []string
	for _, w := range warnings {
		if existing, ok := groupMap[w.Code]; ok {
			existing.NodeIds = append(existing.NodeIds, w.NodeIDs...)
		} else {
			order = append(order, w.Code)
			groupMap[w.Code] = &oas.CompileIssue{
				Code:    oas.NewOptString(w.Code),
				Message: oas.NewOptString(w.Message),
				NodeIds: append([]string(nil), w.NodeIDs...),
			}
		}
	}
	sort.Strings(order)
	out := make([]oas.CompileIssue, 0, len(order))
	for _, code := range order {
		out = append(out, *groupMap[code])
	}
	return out
}

// ══════════════════════════ Import / Export ══════════════════════════

func (h *oapiHandler) ExportAgent(ctx context.Context, params oas.ExportAgentParams) (*oas.ExportAgentOKHeaders, error) {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	gd := agent.StrategyDef.AsGraph()
	if gd == nil {
		return nil, errdefs.Validationf("agent has no graph definition")
	}
	format := params.Format.Or(oas.ExportAgentFormatJSON)
	var data []byte
	switch format {
	case oas.ExportAgentFormatYaml:
		data, err = yaml.Marshal(gd)
	default:
		data, err = json.MarshalIndent(gd, "", "  ")
	}
	if err != nil {
		return nil, errdefs.Internalf("serialize graph: %v", err)
	}
	safeName := strings.NewReplacer(`"`, `'`, `\`, `_`, "\n", "_", "\r", "_").Replace(agent.Name)
	ext := "json"
	if format == oas.ExportAgentFormatYaml {
		ext = "yaml"
	}
	filename := safeName + "." + ext
	return &oas.ExportAgentOKHeaders{
		ContentDisposition: `attachment; filename="` + filename + `"`,
		Response:           oas.ExportAgentOK{Data: bytes.NewReader(data)},
	}, nil
}

func (h *oapiHandler) ImportAgent(ctx context.Context, req *oas.ImportRequest, params oas.ImportAgentParams) (*oas.ImportResult, error) {
	if req.Content == "" {
		return nil, errdefs.Validationf("content is required")
	}
	var imported model.Agent
	switch req.Format {
	case "json":
		if err := json.Unmarshal([]byte(req.Content), &imported); err != nil {
			return nil, errdefs.Validationf("invalid JSON: %v", err)
		}
	case "yaml":
		if err := yaml.Unmarshal([]byte(req.Content), &imported); err != nil {
			return nil, errdefs.Validationf("invalid YAML: %v", err)
		}
	default:
		return nil, errdefs.Validationf("unsupported import format: %s", req.Format)
	}

	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	force, _ := req.Force.Get()
	if imported.StrategyDef.AsGraph() != nil {
		agent.StrategyDef = imported.StrategyDef
	}
	if imported.Name != "" && (force || agent.Name == "") {
		agent.Name = imported.Name
	}
	if imported.Description != "" {
		agent.Description = imported.Description
	}
	if imported.InputSchema != nil {
		agent.InputSchema = imported.InputSchema
	}
	if imported.OutputSchema != nil {
		agent.OutputSchema = imported.OutputSchema
	}
	if force {
		agent.Config = imported.Config
	}

	updated, err := h.s.deps.Platform.Store.UpdateAgent(ctx, agent)
	if err != nil {
		return nil, fmt.Errorf("import update agent: %w", err)
	}
	h.saveDraftVersion(ctx, updated)
	if h.s.deps.Platform.EventBus != nil {
		_ = h.s.deps.Platform.EventBus.Publish(ctx, event.Event{
			Type:    event.EventGraphChanged,
			ActorID: params.ID,
			Payload: map[string]any{"source": "import", "import_id": xid.New().String()},
		})
	}

	result := &oas.ImportResult{Success: oas.NewOptBool(true)}
	if ugd := updated.StrategyDef.AsGraph(); ugd != nil && h.s.deps.Platform.Compiler != nil {
		compiled, compileErr := h.s.deps.Platform.Compiler.Compile(ugd)
		cr := oas.CompileResult{}
		if compileErr != nil {
			cr.Success = oas.NewOptBool(false)
			cr.Errors = []oas.CompileIssue{{Message: oas.NewOptString(compileErr.Error())}}
		} else {
			cr.Success = oas.NewOptBool(true)
			if len(compiled.Warnings) > 0 {
				warnings := make([]oas.CompileIssue, len(compiled.Warnings))
				for i, cw := range compiled.Warnings {
					warnings[i] = oas.CompileIssue{
						Code:    oas.NewOptString(cw.Code),
						Message: oas.NewOptString(cw.Message),
						NodeIds: cw.NodeIDs,
					}
				}
				cr.Warnings = warnings
			}
		}
		result.CompileResult = oas.NewOptCompileResult(cr)
	}
	return result, nil
}

// ══════════════════════════ Memory ══════════════════════════

func (h *oapiHandler) ListMemories(ctx context.Context, params oas.ListMemoriesParams) (*oas.MemoryEntryList, error) {
	if h.s.deps.Platform.LTStore == nil {
		return &oas.MemoryEntryList{Data: []oas.MemoryEntry{}}, nil
	}
	opts := memory.ListOptions{}
	if v, ok := params.Category.Get(); ok {
		opts.Category = memory.MemoryCategory(v)
	}
	var entries []*memory.MemoryEntry
	var err error
	if q, ok := params.Q.Get(); ok && q != "" {
		entries, err = h.s.deps.Platform.LTStore.Search(ctx, ownerRealmID, q, memory.SearchOptions{
			Category: opts.Category,
		})
	} else {
		entries, err = h.s.deps.Platform.LTStore.List(ctx, ownerRealmID, opts)
	}
	if err != nil {
		return nil, err
	}
	oasEntries, err := toOASSlice[oas.MemoryEntry](entries)
	if err != nil {
		return nil, err
	}
	return &oas.MemoryEntryList{Data: oasEntries}, nil
}

func (h *oapiHandler) UpdateMemory(ctx context.Context, req *oas.UpdateMemoryRequest, params oas.UpdateMemoryParams) (*oas.MemoryEntry, error) {
	if h.s.deps.Platform.LTStore == nil {
		return nil, errdefs.Internalf("long-term memory store not configured")
	}
	if req.Content == "" {
		return nil, errdefs.Validationf("content is required")
	}
	entry := &memory.MemoryEntry{
		ID:      params.EntryID,
		Content: req.Content,
	}
	if err := h.s.deps.Platform.LTStore.Update(ctx, ownerRealmID, entry); err != nil {
		return nil, err
	}
	return &oas.MemoryEntry{
		ID:      oas.NewOptString(params.EntryID),
		Content: oas.NewOptString(req.Content),
	}, nil
}

func (h *oapiHandler) DeleteMemory(ctx context.Context, params oas.DeleteMemoryParams) error {
	if h.s.deps.Platform.LTStore == nil {
		return errdefs.Internalf("long-term memory store not configured")
	}
	return h.s.deps.Platform.LTStore.Delete(ctx, ownerRealmID, params.EntryID)
}

// ══════════════════════════ Node Types + Templates ══════════════════════════

func (h *oapiHandler) ListNodeTypes(ctx context.Context) (*oas.NodeTypeList, error) {
	modelOpts := h.buildLLMModelOptions(ctx)
	var schemas []oas.NodeSchema
	if h.s.deps.Platform.SchemaReg != nil {
		for _, ns := range h.s.deps.Platform.SchemaReg.All() {
			if len(modelOpts) > 0 && ns.Type == "llm" {
				nsCopy := ns
				for j := range nsCopy.Fields {
					if nsCopy.Fields[j].Key == "model" {
						nsCopy.Fields[j].Options = modelOpts
						break
					}
				}
				s, err := toOAS[oas.NodeSchema](nsCopy)
				if err != nil {
					continue
				}
				schemas = append(schemas, *s)
				continue
			}
			s, err := toOAS[oas.NodeSchema](ns)
			if err != nil {
				continue
			}
			schemas = append(schemas, *s)
		}
	}
	if h.s.deps.Platform.PluginReg != nil {
		for _, m := range h.s.deps.Platform.PluginReg.CollectNodeSchemas() {
			s, err := toOAS[oas.NodeSchema](m)
			if err != nil {
				continue
			}
			schemas = append(schemas, *s)
		}
	}
	if schemas == nil {
		schemas = []oas.NodeSchema{}
	}
	return &oas.NodeTypeList{Data: schemas}, nil
}

func (h *oapiHandler) buildLLMModelOptions(ctx context.Context) []node.SelectOption {
	configs, err := h.s.deps.Platform.Store.ListProviderConfigs(ctx)
	if err != nil {
		return nil
	}
	options := []node.SelectOption{
		{Value: "", Label: "Use Default Model"},
	}
	for _, c := range configs {
		if !strings.HasPrefix(c.Provider, configuredModelKeyPrefix) {
			continue
		}
		key := c.Provider[len(configuredModelKeyPrefix):]
		options = append(options, node.SelectOption{Value: key, Label: key})
	}
	if len(options) <= 1 {
		return nil
	}
	return options
}

func (h *oapiHandler) ListTemplates(ctx context.Context) (*oas.TemplateList, error) {
	if h.s.deps.Platform.TemplateReg == nil {
		return &oas.TemplateList{Data: []oas.GraphTemplate{}}, nil
	}
	all := h.s.deps.Platform.TemplateReg.All()
	oasTmpls, err := toOASSlice[oas.GraphTemplate](all)
	if err != nil {
		return nil, err
	}
	return &oas.TemplateList{Data: oasTmpls}, nil
}

func (h *oapiHandler) CreateTemplate(ctx context.Context, req *oas.CreateTemplateRequest) (*oas.GraphTemplate, error) {
	if h.s.deps.Platform.TemplateReg == nil {
		return nil, errdefs.Internalf("template registry not configured")
	}
	if req.Name == "" {
		return nil, errdefs.Validationf("name is required")
	}
	var graphDef any
	raw, _ := json.Marshal(req.GraphDef)
	_ = json.Unmarshal(raw, &graphDef)

	saved, err := h.s.deps.Platform.SaveTemplate(ctx, req.Name, req.Label, req.Description, req.Category, graphDef)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.GraphTemplate](saved)
}

func (h *oapiHandler) DeleteTemplate(ctx context.Context, params oas.DeleteTemplateParams) (*oas.DeleteTemplateOK, error) {
	if h.s.deps.Platform.TemplateReg == nil {
		return nil, errdefs.Internalf("template registry not configured")
	}
	if err := h.s.deps.Platform.TemplateReg.Delete(ctx, params.Name); err != nil {
		return nil, err
	}
	return &oas.DeleteTemplateOK{Deleted: oas.NewOptBool(true)}, nil
}

func (h *oapiHandler) InstantiateTemplate(ctx context.Context, req oas.OptTemplateParams, params oas.InstantiateTemplateParams) (*oas.GraphDefinition, error) {
	if h.s.deps.Platform.TemplateReg == nil {
		return nil, errdefs.Internalf("template registry not configured")
	}
	var tmplParams map[string]any
	if v, ok := req.Get(); ok {
		raw, _ := json.Marshal(v)
		_ = json.Unmarshal(raw, &tmplParams)
	}
	result, err := h.s.deps.Platform.InstantiateTemplate(params.Name, tmplParams)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errdefs.NotFoundf("template %q not found", params.Name)
	}
	return toOAS[oas.GraphDefinition](result)
}

// ══════════════════════════ Tools ══════════════════════════

func (h *oapiHandler) ListTools(ctx context.Context) (*oas.ToolList, error) {
	if h.s.deps.Platform.ToolRegistry == nil {
		return &oas.ToolList{Data: []oas.ToolInfo{}}, nil
	}
	var items []oas.ToolInfo
	for _, d := range h.s.deps.Platform.ToolRegistry.DefinitionsByScope(tool.ScopeAgent) {
		items = append(items, oas.ToolInfo{
			Name:        oas.NewOptString(d.Name),
			Description: oas.NewOptString(d.Description),
		})
	}
	if items == nil {
		items = []oas.ToolInfo{}
	}
	return &oas.ToolList{Data: items}, nil
}

// ══════════════════════════ Skills ══════════════════════════

func (h *oapiHandler) ListSkills(ctx context.Context) (*oas.SkillList, error) {
	if h.s.deps.Platform.SkillStore == nil {
		return &oas.SkillList{Data: []oas.SkillInfo{}}, nil
	}
	skills := h.s.deps.Platform.SkillStore.List(nil)
	items := make([]oas.SkillInfo, 0, len(skills))
	for _, sk := range skills {
		entry := oas.SkillInfo{
			Name:        oas.NewOptString(sk.Name),
			Description: oas.NewOptString(sk.Description),
			Entry:       oas.NewOptString(sk.Entry),
			Dir:         oas.NewOptString(sk.Dir),
			Builtin:     oas.NewOptBool(sk.Builtin),
			Enabled:     oas.NewOptBool(h.s.deps.Platform.SkillStore.IsEnabled(sk.Name)),
		}
		if sk.Tags != nil {
			entry.Tags = sk.Tags
		}
		if src := h.s.deps.Platform.SkillStore.GetSource(sk.Name); src != nil {
			entry.Source = oas.NewOptString(src.GitURL)
		}
		items = append(items, entry)
	}
	return &oas.SkillList{Data: items}, nil
}

func (h *oapiHandler) InstallSkill(ctx context.Context, req *oas.InstallSkillRequest) (*oas.SkillInstallResult, error) {
	if h.s.deps.Platform.SkillStore == nil {
		return nil, errdefs.Internalf("skill store not configured")
	}
	if req.URL == "" {
		return nil, errdefs.Validationf("url is required")
	}
	name := req.Name.Or("")
	if err := h.s.deps.Platform.SkillStore.Install(ctx, req.URL, name); err != nil {
		return nil, err
	}
	return &oas.SkillInstallResult{
		Status: oas.NewOptString("installed"),
		URL:    oas.NewOptString(req.URL),
		Name:   oas.NewOptString(name),
	}, nil
}

func (h *oapiHandler) UpdateSkill(ctx context.Context, params oas.UpdateSkillParams) (*oas.UpdateSkillOK, error) {
	if h.s.deps.Platform.SkillStore == nil {
		return nil, errdefs.Internalf("skill store not configured")
	}
	if err := h.s.deps.Platform.SkillStore.Update(ctx, params.Name); err != nil {
		return nil, err
	}
	return &oas.UpdateSkillOK{
		Status: oas.NewOptString("updated"),
		Name:   oas.NewOptString(params.Name),
	}, nil
}

func (h *oapiHandler) UpdateAllSkills(ctx context.Context) (*oas.UpdateAllSkillsOK, error) {
	if h.s.deps.Platform.SkillStore == nil {
		return nil, errdefs.Internalf("skill store not configured")
	}
	updated, err := h.s.deps.Platform.SkillStore.UpdateAll(ctx)
	if err != nil {
		return nil, err
	}
	return &oas.UpdateAllSkillsOK{Updated: updated}, nil
}

func (h *oapiHandler) DeleteSkill(ctx context.Context, params oas.DeleteSkillParams) error {
	if h.s.deps.Platform.SkillStore == nil {
		return errdefs.Internalf("skill store not configured")
	}
	if h.s.deps.Platform.SkillStore.IsBuiltin(params.Name) {
		return errdefs.Validationf("cannot uninstall built-in skill %q", params.Name)
	}
	return h.s.deps.Platform.SkillStore.Uninstall(ctx, params.Name)
}

// ══════════════════════════ Models + Providers ══════════════════════════

func (h *oapiHandler) ListModels(ctx context.Context) (*oas.ModelList, error) {
	allModels := llm.ListAllModels()
	defaultProvider, defaultModel := h.resolveDefaultModel(ctx)
	items := make([]oas.ModelInfo, 0, len(allModels))
	for _, m := range allModels {
		items = append(items, oas.ModelInfo{
			Provider:  oas.NewOptString(m.Provider),
			Model:     oas.NewOptString(m.Name),
			Label:     oas.NewOptString(m.Label),
			IsDefault: oas.NewOptBool(m.Provider == defaultProvider && m.Name == defaultModel),
		})
	}
	return &oas.ModelList{Data: items}, nil
}

func (h *oapiHandler) AddModel(ctx context.Context, req *oas.AddModelRequest) (*oas.ModelInfo, error) {
	if req.Provider == "" || req.Model == "" {
		return nil, errdefs.Validationf("provider and model are required")
	}
	cfg := map[string]any{"model": req.Model}
	if v, ok := req.APIKey.Get(); ok {
		cfg["api_key"] = v
	}
	if v, ok := req.BaseURL.Get(); ok {
		cfg["base_url"] = v
	}
	if req.Extra != nil {
		raw, _ := json.Marshal(req.Extra)
		var extra map[string]any
		_ = json.Unmarshal(raw, &extra)
		for k, ev := range extra {
			cfg[k] = ev
		}
	}
	pc := &llm.ProviderConfig{Provider: req.Provider, Config: cfg}
	if err := h.s.deps.Platform.Store.SetProviderConfig(ctx, pc); err != nil {
		return nil, err
	}
	return &oas.ModelInfo{
		Provider: oas.NewOptString(req.Provider),
		Model:    oas.NewOptString(req.Model),
	}, nil
}

func (h *oapiHandler) SetDefaultModel(ctx context.Context, req *oas.SetDefaultModelRequest) error {
	if req.Provider == "" || req.Model == "" {
		return errdefs.Validationf("provider and model are required")
	}
	pc := &llm.ProviderConfig{
		Provider: llm.GlobalDefaultProvider,
		Config:   map[string]any{"provider": req.Provider, "model": req.Model},
	}
	if err := h.s.deps.Platform.Store.SetProviderConfig(ctx, pc); err != nil {
		return err
	}
	if h.s.deps.Platform.LLMResolver != nil {
		h.s.deps.Platform.LLMResolver.InvalidateCache("")
	}
	return nil
}

func (h *oapiHandler) DeleteModel(ctx context.Context, params oas.DeleteModelParams) error {
	id := params.ModelID
	if id == "" {
		return errdefs.Validationf("model id is required")
	}
	storeKey := configuredModelKeyPrefix + id
	if err := h.s.deps.Platform.Store.DeleteProviderConfig(ctx, storeKey); err != nil {
		return err
	}
	if gc, err := h.s.deps.Platform.Store.GetProviderConfig(ctx, llm.GlobalDefaultProvider); err == nil {
		if p, _ := gc.Config["provider"].(string); p != "" {
			if m, _ := gc.Config["model"].(string); m != "" {
				if p+"/"+m == id {
					_ = h.s.deps.Platform.Store.DeleteProviderConfig(ctx, llm.GlobalDefaultProvider)
				}
			}
		}
	}
	if h.s.deps.Platform.LLMResolver != nil {
		provider, _ := splitProviderModelPath(id)
		h.s.deps.Platform.LLMResolver.InvalidateCache(provider)
	}
	return nil
}

func splitProviderModelPath(s string) (provider, modelName string) {
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}

func (h *oapiHandler) ListProviders(ctx context.Context) (*oas.ProviderList, error) {
	providers := llm.ListProviders()
	allModels := llm.ListAllModels()
	configured := make(map[string]bool)
	configs, err := h.s.deps.Platform.Store.ListProviderConfigs(ctx)
	if err == nil {
		for _, pc := range configs {
			if pc.Provider != llm.GlobalDefaultProvider {
				configured[pc.Provider] = true
			}
		}
	}
	modelsByProvider := make(map[string][]oas.ProviderInfoModelsItem)
	for _, m := range allModels {
		modelsByProvider[m.Provider] = append(modelsByProvider[m.Provider], oas.ProviderInfoModelsItem{
			Name:  oas.NewOptString(m.Name),
			Label: oas.NewOptString(m.Label),
		})
	}
	items := make([]oas.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		items = append(items, oas.ProviderInfo{
			Name:       oas.NewOptString(p),
			Configured: oas.NewOptBool(configured[p]),
			Models:     modelsByProvider[p],
		})
	}
	return &oas.ProviderList{Data: items}, nil
}

func (h *oapiHandler) ConfigureProvider(ctx context.Context, req *oas.ConfigureProviderRequest, params oas.ConfigureProviderParams) error {
	if req.APIKey == "" {
		return errdefs.Validationf("api_key is required")
	}
	cfg := map[string]any{"api_key": req.APIKey}
	if v, ok := req.BaseURL.Get(); ok {
		cfg["base_url"] = v
	}
	if req.Extra != nil {
		raw, _ := json.Marshal(req.Extra)
		var extra map[string]any
		_ = json.Unmarshal(raw, &extra)
		for k, ev := range extra {
			cfg[k] = ev
		}
	}
	pc := &llm.ProviderConfig{Provider: params.Name, Config: cfg}
	if err := h.s.deps.Platform.Store.SetProviderConfig(ctx, pc); err != nil {
		return err
	}
	if h.s.deps.Platform.LLMResolver != nil {
		h.s.deps.Platform.LLMResolver.InvalidateCache("")
	}
	return nil
}

func (h *oapiHandler) resolveDefaultModel(ctx context.Context) (provider, mdl string) {
	gc, err := h.s.deps.Platform.Store.GetProviderConfig(ctx, llm.GlobalDefaultProvider)
	if err != nil {
		return "", ""
	}
	if p, _ := gc.Config["provider"].(string); p != "" {
		provider = p
	}
	if m, _ := gc.Config["model"].(string); m != "" {
		mdl = m
	}
	return
}

// ══════════════════════════ Stats ══════════════════════════

func (h *oapiHandler) GetStats(ctx context.Context) (*oas.StatsOverview, error) {
	stats, err := h.s.deps.Platform.Store.GetStats(ctx)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.StatsOverview](stats)
}

func (h *oapiHandler) GetRunStats(ctx context.Context, params oas.GetRunStatsParams) (*oas.DailyRunStatsList, error) {
	agentID := params.AgentID.Or("")
	days := params.Days.Or(30)
	stats, err := h.s.deps.Platform.Store.ListDailyRunStats(ctx, agentID, days)
	if err != nil {
		return nil, err
	}
	oasStats, err := toOASSlice[oas.DailyRunStats](stats)
	if err != nil {
		return nil, err
	}
	return &oas.DailyRunStatsList{Data: oasStats}, nil
}

func (h *oapiHandler) GetRuntimeStats(ctx context.Context) (*oas.RuntimeStats, error) {
	stats := h.s.deps.Platform.RealmStats()
	return toOAS[oas.RuntimeStats](stats)
}

func (h *oapiHandler) GetMemoryStats(ctx context.Context) (*oas.MemoryStats, error) {
	if h.s.deps.Platform.LTStore == nil {
		return &oas.MemoryStats{TotalEntries: oas.NewOptInt(0)}, nil
	}
	entries, err := h.s.deps.Platform.LTStore.List(ctx, ownerRealmID, memory.ListOptions{})
	if err != nil {
		return nil, err
	}
	return &oas.MemoryStats{
		RuntimeID:    oas.NewOptString(ownerRealmID),
		TotalEntries: oas.NewOptInt(len(entries)),
	}, nil
}

// ══════════════════════════ Monitoring ══════════════════════════

func (h *oapiHandler) GetMonitoringSummary(ctx context.Context, params oas.GetMonitoringSummaryParams) (*oas.MonitoringSummary, error) {
	agentID := params.AgentID.Or("")
	windowStr := "24h"
	if v, ok := params.Window.Get(); ok {
		windowStr = string(v)
	}
	_, since, err := parseMonitoringWindowStrict(windowStr)
	if err != nil {
		return nil, errdefs.Validationf("monitoring summary: %v", err)
	}
	summary, err := h.s.deps.Platform.Store.GetMonitoringSummary(ctx, agentID, since)
	if err != nil {
		return nil, err
	}
	cfg := h.s.resolvedMonitoringConfig()
	summary.Thresholds = model.MonitoringThresholds{
		ErrorRateWarn:        cfg.ErrorRateWarn,
		ErrorRateDown:        cfg.ErrorRateDown,
		LatencyP95WarnMs:     cfg.LatencyP95WarnMs,
		ConsecutiveBuckets:   cfg.ConsecutiveBuckets,
		NoSuccessDownMinutes: cfg.NoSuccessDownMinutes,
	}
	rt := h.s.deps.Platform.RealmStats().Current
	if rt != nil {
		summary.ActiveActors = rt.ActorCount
		summary.ActiveSandboxes = rt.SandboxLeases
	}
	recentSince := time.Now().UTC().Add(-time.Duration(cfg.NoSuccessDownMinutes) * time.Minute)
	recentSummary, err := h.s.deps.Platform.Store.GetMonitoringSummary(ctx, agentID, recentSince)
	if err != nil {
		return nil, err
	}
	hasRecentFailureWithoutSuccess := recentSummary.RunFailed > 0 && recentSummary.RunSuccess == 0
	classifyMonitoringHealth(summary, cfg, hasRecentFailureWithoutSuccess)
	return toOAS[oas.MonitoringSummary](summary)
}

func (h *oapiHandler) GetMonitoringTimeseries(ctx context.Context, params oas.GetMonitoringTimeseriesParams) (*oas.MonitoringTimeseries, error) {
	agentID := params.AgentID.Or("")
	windowStr := "24h"
	if v, ok := params.Window.Get(); ok && v != "" {
		windowStr = v
	}
	_, since, err := parseMonitoringWindowStrict(windowStr)
	if err != nil {
		return nil, errdefs.Validationf("monitoring timeseries: %v", err)
	}
	intervalStr := ""
	if v, ok := params.Interval.Get(); ok {
		intervalStr = string(v)
	}
	interval, err := parseMonitoringIntervalStrict(windowStr, intervalStr)
	if err != nil {
		return nil, errdefs.Validationf("monitoring timeseries: %v", err)
	}
	points, err := h.s.deps.Platform.Store.ListMonitoringTimeseries(ctx, agentID, since, interval)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(points)
	var data []oas.MonitoringTimeseriesDataItem
	_ = json.Unmarshal(raw, &data)
	return &oas.MonitoringTimeseries{Data: data}, nil
}

func (h *oapiHandler) GetMonitoringRuntime(ctx context.Context) (*oas.RuntimeManagerStats, error) {
	stats := h.s.deps.Platform.RealmStats()
	return toOAS[oas.RuntimeManagerStats](stats)
}

func (h *oapiHandler) GetMonitoringDiagnostics(ctx context.Context, params oas.GetMonitoringDiagnosticsParams) (*oas.MonitoringDiagnostics, error) {
	agentID := params.AgentID.Or("")
	windowStr := "24h"
	if v, ok := params.Window.Get(); ok && v != "" {
		windowStr = v
	}
	_, since, err := parseMonitoringWindowStrict(windowStr)
	if err != nil {
		return nil, errdefs.Validationf("monitoring diagnostics: %v", err)
	}
	limit := clampMonitoringLimit(params.Limit.Or(20))
	diag, err := h.s.deps.Platform.Store.GetMonitoringDiagnostics(ctx, agentID, since, limit)
	if err != nil {
		return nil, err
	}
	return toOAS[oas.MonitoringDiagnostics](diag)
}

// ══════════════════════════ Plugins ══════════════════════════

func (h *oapiHandler) ListPlugins(ctx context.Context, params oas.ListPluginsParams) (*oas.PluginList, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return &oas.PluginList{Data: []oas.PluginDetail{}}, nil
	}
	all := h.s.deps.Platform.PluginReg.List()
	if filterType, ok := params.Type.Get(); ok && filterType != "" {
		filtered := make([]any, 0)
		for _, p := range all {
			raw, _ := json.Marshal(p)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if t, _ := m["type"].(string); t == filterType {
				filtered = append(filtered, p)
			}
		}
		items, err := toOASSlice[oas.PluginDetail](filtered)
		if err != nil {
			return nil, err
		}
		return &oas.PluginList{Data: items}, nil
	}
	items, err := toOASSlice[oas.PluginDetail](all)
	if err != nil {
		return nil, err
	}
	return &oas.PluginList{Data: items}, nil
}

func (h *oapiHandler) GetPlugin(ctx context.Context, params oas.GetPluginParams) (*oas.PluginDetail, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	p, ok := h.s.deps.Platform.PluginReg.Get(params.Name)
	if !ok {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	return toOAS[oas.PluginDetail](p.Info())
}

func (h *oapiHandler) EnablePlugin(ctx context.Context, req oas.OptPluginConfig, params oas.EnablePluginParams) (*oas.PluginInfo, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	var config map[string]any
	if v, ok := req.Get(); ok {
		raw, _ := json.Marshal(v)
		_ = json.Unmarshal(raw, &config)
	}
	if err := h.s.deps.Platform.PluginReg.Enable(ctx, params.Name, config); err != nil {
		return nil, err
	}
	return &oas.PluginInfo{Name: oas.NewOptString(params.Name)}, nil
}

func (h *oapiHandler) DisablePlugin(ctx context.Context, params oas.DisablePluginParams) (*oas.PluginInfo, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	if err := h.s.deps.Platform.PluginReg.Disable(ctx, params.Name); err != nil {
		return nil, err
	}
	return &oas.PluginInfo{Name: oas.NewOptString(params.Name)}, nil
}

func (h *oapiHandler) UpdatePluginConfig(ctx context.Context, req oas.PluginConfig, params oas.UpdatePluginConfigParams) (*oas.PluginInfo, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	var config map[string]any
	raw, _ := json.Marshal(req)
	_ = json.Unmarshal(raw, &config)
	if err := h.s.deps.Platform.PluginReg.UpdateConfig(ctx, params.Name, config); err != nil {
		return nil, err
	}
	return &oas.PluginInfo{Name: oas.NewOptString(params.Name)}, nil
}

func (h *oapiHandler) ReloadPlugins(ctx context.Context) (*oas.ReloadPluginsOK, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return &oas.ReloadPluginsOK{}, nil
	}
	added, removed, err := h.s.deps.Platform.PluginReg.Reload(ctx)
	if err != nil {
		return nil, err
	}
	h.s.deps.Platform.SyncPluginSchemas()
	return &oas.ReloadPluginsOK{
		Added:   make([]string, added),
		Removed: make([]string, removed),
	}, nil
}

func (h *oapiHandler) UploadPlugin(ctx context.Context, req *oas.UploadPluginReq) (*oas.PluginUploadResult, error) {
	return nil, errdefs.Forbiddenf("plugin upload not supported via API")
}

func (h *oapiHandler) DeletePlugin(ctx context.Context, params oas.DeletePluginParams) error {
	return errdefs.Forbiddenf("plugin deletion not supported via API; remove the binary and call POST /plugins/reload")
}

// ══════════════════════════ Kanban ══════════════════════════

func lookupRuntimeBoard(plat *platform.Platform) (*kanban.TaskBoard, bool) {
	board := plat.TaskBoard()
	return board, board != nil
}

func (h *oapiHandler) GetKanbanCards(ctx context.Context) (*oas.KanbanCardList, error) {
	if board, ok := lookupRuntimeBoard(h.s.deps.Platform); ok {
		raw, _ := json.Marshal(board.Cards())
		var data []oas.KanbanCardListDataItem
		_ = json.Unmarshal(raw, &data)
		return &oas.KanbanCardList{Data: data}, nil
	}
	return &oas.KanbanCardList{Data: []oas.KanbanCardListDataItem{}}, nil
}

func (h *oapiHandler) GetKanbanTimeline(ctx context.Context) (*oas.KanbanTimeline, error) {
	if board, ok := lookupRuntimeBoard(h.s.deps.Platform); ok {
		raw, _ := json.Marshal(board.Timeline())
		var data []oas.KanbanTimelineDataItem
		_ = json.Unmarshal(raw, &data)
		return &oas.KanbanTimeline{Data: data}, nil
	}
	return &oas.KanbanTimeline{Data: []oas.KanbanTimelineDataItem{}}, nil
}

func (h *oapiHandler) GetKanbanTopology(ctx context.Context) (*oas.KanbanTopology, error) {
	if board, ok := lookupRuntimeBoard(h.s.deps.Platform); ok {
		topo := board.Topology()
		raw, _ := json.Marshal(topo)
		var result oas.KanbanTopology
		_ = json.Unmarshal(raw, &result)
		return &result, nil
	}
	return &oas.KanbanTopology{
		Nodes: []oas.KanbanTopologyNodesItem{},
		Edges: []oas.KanbanTopologyEdgesItem{},
	}, nil
}

// ══════════════════════════ Channel Types ══════════════════════════

func channelTypeConfigSchema(props map[string]any) oas.OptChannelTypeConfigSchema {
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return oas.OptChannelTypeConfigSchema{}
	}
	var cs oas.ChannelTypeConfigSchema
	if err := json.Unmarshal(b, &cs); err != nil {
		return oas.OptChannelTypeConfigSchema{}
	}
	return oas.NewOptChannelTypeConfigSchema(cs)
}

func (h *oapiHandler) GetChannelTypes(ctx context.Context) (*oas.ChannelTypeList, error) {
	types := []oas.ChannelType{
		{
			Type:  oas.NewOptString("slack"),
			Label: oas.NewOptString("Slack"),
			ConfigSchema: channelTypeConfigSchema(map[string]any{
				"signing_secret": map[string]string{"type": "string", "title": "Signing Secret"},
				"bot_token":      map[string]string{"type": "string", "title": "Bot Token"},
			}),
		},
		{
			Type:  oas.NewOptString("dingtalk"),
			Label: oas.NewOptString("钉钉"),
			ConfigSchema: channelTypeConfigSchema(map[string]any{
				"app_secret":  map[string]string{"type": "string", "title": "App Secret"},
				"webhook_url": map[string]string{"type": "string", "title": "Webhook URL"},
			}),
		},
		{
			Type:  oas.NewOptString("feishu"),
			Label: oas.NewOptString("飞书 / Lark"),
			ConfigSchema: channelTypeConfigSchema(map[string]any{
				"verification_token": map[string]string{"type": "string", "title": "Verification Token"},
				"encrypt_key":        map[string]string{"type": "string", "title": "Encrypt Key"},
				"app_id":             map[string]string{"type": "string", "title": "App ID"},
				"app_secret":         map[string]string{"type": "string", "title": "App Secret"},
			}),
		},
	}
	return &oas.ChannelTypeList{Data: types}, nil
}

// ══════════════════════════ Chat SSE ══════════════════════════

func (h *oapiHandler) ChatStream(ctx context.Context, req *oas.ChatRequest) (oas.ChatStreamOK, error) {
	httpReq, _ := HTTPRequestFromContext(ctx)
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		h.writeChatSSE(ctx, httpReq, req, pw)
	}()
	return oas.ChatStreamOK{Data: pr}, nil
}

func (h *oapiHandler) writeChatSSE(ctx context.Context, httpReq *http.Request, req *oas.ChatRequest, w io.Writer) {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, req.AgentID)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"code\":\"agent_not_found\",\"message\":%q}\n\n", err.Error())
		return
	}

	var inputs map[string]any
	if req.Inputs != nil {
		raw, _ := json.Marshal(req.Inputs)
		_ = json.Unmarshal(raw, &inputs)
	}
	if agent.Type == model.AgentTypeCoPilot && httpReq != nil {
		compat := chatRequestCompat{
			AgentID: req.AgentID,
			Query:   req.Query,
			Inputs:  inputs,
		}
		if conv, ok := req.ConversationID.Get(); ok {
			compat.ConversationID = conv
		}
		if async, ok := req.Async.Get(); ok {
			compat.Async = async
		}
		inputs = h.s.buildCoPilotInputsFromChat(httpReq, agent, compat)
	}
	convID := req.ConversationID.Or("")
	if convID == "" {
		convID = ownerRealmID + "--" + agent.AgentID
	}
	wfReq := &workflow.Request{
		ContextID: convID,
		RuntimeID: ownerRealmID,
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, req.Query),
		Inputs:    inputs,
	}

	persistent := agent.Type == model.AgentTypeCoPilot
	doneCh, sub, _, err := h.s.deps.Platform.RunAgentStreaming(ctx, agent, wfReq, persistent)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"code\":\"runtime_error\",\"message\":%q}\n\n", err.Error())
		return
	}
	defer func() { _ = sub.Close() }()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Events():
			if !ok {
				return
			}
			raw, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, string(raw))
		case result := <-doneCh:
			if result.Err != nil {
				fmt.Fprintf(w, "event: error\ndata: {\"code\":\"run_error\",\"message\":%q}\n\n", result.Err.Error())
			} else {
				raw, _ := json.Marshal(result.Value)
				fmt.Fprintf(w, "event: done\ndata: %s\n\n", string(raw))
			}
			return
		}
	}
}

func (h *oapiHandler) ResumeStream(ctx context.Context, req *oas.ResumeRequest) (oas.ResumeStreamOK, error) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		h.writeResumeSSE(ctx, req, pw)
	}()
	return oas.ResumeStreamOK{Data: pr}, nil
}

func (h *oapiHandler) writeResumeSSE(ctx context.Context, req *oas.ResumeRequest, w io.Writer) {
	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, req.AgentID)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"code\":\"agent_not_found\",\"message\":%q}\n\n", err.Error())
		return
	}

	var snap *graph.BoardSnapshot
	if req.State != nil {
		raw, _ := json.Marshal(req.State)
		snap = new(graph.BoardSnapshot)
		_ = json.Unmarshal(raw, snap)
	}
	if snap == nil {
		loaded, loadErr := h.s.deps.Platform.LoadCheckpoint(ctx, req.AgentID)
		if loadErr != nil || loaded == nil {
			fmt.Fprintf(w, "event: error\ndata: {\"code\":\"no_state\",\"message\":\"no state provided and no checkpoint available\"}\n\n")
			return
		}
		snap = loaded
	}

	var decision map[string]any
	if req.Decision != (oas.ResumeRequestDecision{}) {
		raw, _ := json.Marshal(req.Decision)
		_ = json.Unmarshal(raw, &decision)
	}

	wfReq := &workflow.Request{
		ContextID: req.ConversationID,
		RunID:     req.RunID,
		Inputs:    decision,
	}

	startNode := graph.RestoreBoard(snap).GetVarString(graph.VarInterruptedNode)

	result, resumeErr := h.s.deps.Platform.ResumeAgent(ctx, agent, wfReq, snap, startNode)
	if resumeErr != nil {
		fmt.Fprintf(w, "event: error\ndata: {\"code\":\"run_error\",\"message\":%q}\n\n", resumeErr.Error())
	} else {
		raw, _ := json.Marshal(result)
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", string(raw))
	}
}

// ══════════════════════════ WS Ticket ══════════════════════════

func (h *oapiHandler) CreateWSTicket(ctx context.Context) (*oas.WSTicketResponse, error) {
	ticket, expiresAt, err := h.s.wsTickets.issue(wsTicketTTL)
	if err != nil {
		return nil, errdefs.Internalf("failed to issue websocket ticket")
	}
	return &oas.WSTicketResponse{
		Ticket:    ticket,
		ExpiresAt: expiresAt,
	}, nil
}

// ══════════════════════════ Helpers ══════════════════════════

func (h *oapiHandler) saveDraftVersion(ctx context.Context, agent *model.Agent) {
	gd := agent.StrategyDef.AsGraph()
	if h.s.deps.Platform.VersionStore == nil || gd == nil {
		return
	}
	gv := &model.GraphVersion{
		AgentID:  agent.AgentID,
		Version:  1,
		GraphDef: gd,
	}
	_ = h.s.deps.Platform.VersionStore.SaveDraft(ctx, gv)
}

func (h *oapiHandler) publishAgentConfigChanged(ctx context.Context, agentID string) {
	if h.s.deps.Platform.EventBus == nil {
		return
	}
	_ = h.s.deps.Platform.EventBus.Publish(ctx, event.Event{
		Type:    event.EventAgentConfigChanged,
		ActorID: agentID,
	})
}
