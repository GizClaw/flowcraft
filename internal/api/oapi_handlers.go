package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/GizClaw/flowcraft/internal/api/oas"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	sdkmodel "github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	"github.com/rs/xid"
	otellog "go.opentelemetry.io/otel/log"
	"gopkg.in/yaml.v3"
)

// As of migration 005 the model-config storage layout is split into
// three tables:
//
//   - provider_configs holds ONLY plain provider credentials
//     (api_key, base_url, ...) keyed by provider name. Read by
//     sdk/llm.DefaultResolver via store.GetProviderConfig.
//   - model_configs holds per-model {Caps, Extra} overrides keyed by
//     (provider, model). Read by the resolver via the optional
//     llm.ModelConfigStore interface (store.GetModelConfig).
//   - default_model is a singleton row carrying the current default
//     {provider, model} pair, read via the optional
//     llm.DefaultModelStore interface (store.GetDefaultModel).
//
// Handlers no longer encode/decode magic PK prefixes; each store
// method owns its own typed row.

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

// decodeOptJSONObject decodes an OptJSONObject (the ogen-generated wrapper for
// `JSONObject`-typed schema fields) into the destination via JSON round-trip.
// Returns false (and leaves dst untouched) when the option is not set.
func decodeOptJSONObject(opt oas.OptJSONObject, dst any) bool {
	if !opt.Set {
		return false
	}
	raw, err := json.Marshal(opt.Value)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false
	}
	return true
}

// toJSONObject re-encodes any value as oas.JSONObject (map[string]jx.Raw).
// Used for kanban / monitoring payloads where the underlying SDK types are
// not declared in openapi.yaml — the OpenAPI surface exposes them as opaque
// JSON objects.
func toJSONObject(v any) oas.JSONObject {
	raw, err := json.Marshal(v)
	if err != nil {
		return oas.JSONObject{}
	}
	out := oas.JSONObject{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return oas.JSONObject{}
	}
	return out
}

// jsonObjectsFromAny converts a slice-shaped value into []oas.JSONObject by
// JSON round-tripping through []map[string]jx.Raw.
func jsonObjectsFromAny(v any) []oas.JSONObject {
	raw, err := json.Marshal(v)
	if err != nil {
		return []oas.JSONObject{}
	}
	var out []oas.JSONObject
	if err := json.Unmarshal(raw, &out); err != nil {
		return []oas.JSONObject{}
	}
	if out == nil {
		return []oas.JSONObject{}
	}
	return out
}

// ══════════════════════════ Health ══════════════════════════

func (h *oapiHandler) HealthCheck(ctx context.Context) (*oas.HealthStatus, error) {
	return &oas.HealthStatus{Status: "ok"}, nil
}

// ══════════════════════════ Auth ══════════════════════════
//
// All auth flows live behind the OpenAPI surface (see openapi.yaml > tag:auth).
// They share these invariants:
//   • `/auth/setup`, `/auth/login`, `/auth/logout`, `/auth/session`,
//     `/auth/status` are public (no JWT required to reach them).
//   • Successful login sets the `flowcraft_token` HttpOnly cookie via the
//     ResponseWriter pulled from ctx; logout clears it.
//   • When `h.s.jwt == nil`, auth is "open mode": all endpoints are reachable
//     without credentials. Login/setup/change-password short-circuit.

func (h *oapiHandler) GetAuthStatus(ctx context.Context) (*oas.AuthStatus, error) {
	_, err := h.s.deps.Platform.Store.GetOwnerCredential(ctx)
	return &oas.AuthStatus{
		Initialized: err == nil,
		AuthMode:    "jwt",
	}, nil
}

func (h *oapiHandler) SetupAuth(ctx context.Context, req *oas.SetupRequest) (*oas.OkResponse, error) {
	cred, _ := h.s.deps.Platform.Store.GetOwnerCredential(ctx)
	if cred != nil {
		return nil, errdefs.Conflictf("already initialized")
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, errdefs.Validationf("username is required")
	}
	if len(req.Password) < 8 {
		return nil, errdefs.Validationf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		return nil, errdefs.Internalf("failed to hash password")
	}
	if err := h.s.deps.Platform.Store.SetOwnerCredential(ctx, &model.OwnerCredential{
		Username:     username,
		PasswordHash: string(hash),
	}); err != nil {
		return nil, err
	}
	return &oas.OkResponse{Ok: true}, nil
}

func (h *oapiHandler) Login(ctx context.Context, req *oas.LoginRequest) (*oas.LoginResponse, error) {
	if h.s.jwt == nil {
		// Open mode: issue a placeholder response so the SPA's login form
		// "succeeds" without bringing up the auth machinery.
		return &oas.LoginResponse{
			Token:     "",
			ExpiresAt: time.Time{},
		}, nil
	}
	cred, err := h.s.deps.Platform.Store.GetOwnerCredential(ctx)
	if err != nil {
		return nil, errdefs.Unauthorizedf("invalid credentials")
	}
	if cred.Username != req.Username {
		return nil, errdefs.Unauthorizedf("invalid credentials")
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.Password)) != nil {
		return nil, errdefs.Unauthorizedf("invalid credentials")
	}
	token, expiresAt, err := h.s.jwt.Issue(cred.Username)
	if err != nil {
		return nil, errdefs.Internalf("failed to issue token")
	}
	if w, wOK := HTTPResponseWriterFromContext(ctx); wOK {
		rr, _ := HTTPRequestFromContext(ctx)
		secure := rr != nil && rr.TLS != nil
		h.s.setAuthCookie(w, token, expiresAt, secure)
	}
	return &oas.LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
	}, nil
}

func (h *oapiHandler) Logout(ctx context.Context) error {
	if w, wOK := HTTPResponseWriterFromContext(ctx); wOK {
		rr, _ := HTTPRequestFromContext(ctx)
		secure := rr != nil && rr.TLS != nil
		h.s.clearAuthCookie(w, secure)
	}
	return nil
}

func (h *oapiHandler) GetSession(ctx context.Context) (*oas.SessionInfo, error) {
	rr, ok := HTTPRequestFromContext(ctx)
	if !ok || rr == nil {
		return &oas.SessionInfo{Authenticated: false}, nil
	}
	claims, authed := h.s.authenticateRequest(rr)
	if !authed || claims == nil {
		return &oas.SessionInfo{Authenticated: false}, nil
	}
	return &oas.SessionInfo{
		Authenticated: true,
		Username:      oas.NewOptString(claims.Username),
	}, nil
}

func (h *oapiHandler) ChangePassword(ctx context.Context, req *oas.ChangePasswordRequest) (*oas.OkResponse, error) {
	cred, err := h.s.deps.Platform.Store.GetOwnerCredential(ctx)
	if err != nil {
		return nil, errdefs.Unauthorizedf("not initialized")
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.OldPassword)) != nil {
		return nil, errdefs.Unauthorizedf("invalid old password")
	}
	if len(req.NewPassword) < 8 {
		return nil, errdefs.Validationf("new password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), 12)
	if err != nil {
		return nil, errdefs.Internalf("failed to hash password")
	}
	cred.PasswordHash = string(hash)
	if err := h.s.deps.Platform.Store.SetOwnerCredential(ctx, cred); err != nil {
		return nil, err
	}
	return &oas.OkResponse{Ok: true}, nil
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

	var cfg model.AgentConfig
	if decodeOptJSONObject(req.Config, &cfg) {
		agent.Config = cfg
	}
	decodeOptJSONObject(req.InputSchema, &agent.InputSchema)

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
	} else {
		var gd model.GraphDefinition
		if decodeOptJSONObject(req.GraphDefinition, &gd) {
			agent.StrategyDef = model.NewGraphStrategy(&gd)
		}
	}

	created, err := h.s.deps.Platform.Store.CreateAgent(ctx, agent)
	if err != nil {
		return nil, err
	}
	h.saveDraftVersion(ctx, created)
	h.publishAgentConfigChanged(ctx, created.AgentID, "created")
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
	var newCfg model.AgentConfig
	if decodeOptJSONObject(req.Config, &newCfg) {
		agent.Config = newCfg
	}
	var gd model.GraphDefinition
	if decodeOptJSONObject(req.GraphDefinition, &gd) {
		agent.StrategyDef = model.NewGraphStrategy(&gd)
	}
	decodeOptJSONObject(req.InputSchema, &agent.InputSchema)
	agent.UpdatedAt = time.Now()

	updated, err := h.s.deps.Platform.Store.UpdateAgent(ctx, agent)
	if err != nil {
		return nil, err
	}
	h.saveDraftVersion(ctx, updated)
	h.publishAgentConfigChanged(ctx, updated.AgentID, "updated")
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

func (h *oapiHandler) AbortActor(ctx context.Context, params oas.AbortActorParams) (*oas.AbortResult, error) {
	aborted, err := h.s.deps.Platform.AbortAgent(ctx, params.AgentID)
	if err != nil {
		return nil, err
	}
	if !aborted {
		return nil, errdefs.NotFoundf("actor %s not found or not running", params.AgentID)
	}
	return &oas.AbortResult{Aborted: oas.NewOptBool(true)}, nil
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

// GetMessages returns the materialised messages for a conversation. After
// R5 the legacy `messages` table is gone — the ChatProjector is the only
// source of truth, populated from `chat.message.sent` envelopes.
func (h *oapiHandler) GetMessages(ctx context.Context, params oas.GetMessagesParams) (*oas.MessageList, error) {
	if h.s.deps.ChatRead == nil {
		return &oas.MessageList{Data: []oas.Message{}}, nil
	}
	conv := h.s.deps.ChatRead.GetConversation(params.ID)
	if conv == nil {
		return &oas.MessageList{Data: []oas.Message{}}, nil
	}
	out := make([]oas.Message, 0, len(conv.Messages))
	for _, m := range conv.Messages {
		out = append(out, oas.Message{
			ID:             oas.NewOptString(m.MessageID),
			ConversationID: oas.NewOptString(conv.ID),
			Role:           oas.NewOptString(m.Role),
			Content:        oas.NewOptString(m.Content),
			CreatedAt:      oas.NewOptDateTime(m.SentAt),
		})
	}
	return &oas.MessageList{Data: out}, nil
}

// SubmitApproval is the command-side replacement of POST /chat/resume/stream
// (deprecated). The HTTP path carries the conversation/card id; the body carries
// the agent + run identifiers so we can locate the paused checkpoint, plus the
// approval verdict. We synchronously resume the agent (so the caller knows
// whether the resume actually started) but no longer SSE-stream the resulting
// agent activity back through this endpoint — those events flow through the
// shared event log and clients pick them up via
// `GET /api/events?partition=card:{id}&since={last_seq}`.
func (h *oapiHandler) SubmitApproval(ctx context.Context, req *oas.ApprovalDecisionRequest, params oas.SubmitApprovalParams) (*oas.ApprovalDecisionResponse, error) {
	if req == nil {
		return nil, errdefs.Validationf("approval request body required")
	}
	if req.AgentID == "" || req.RunID == "" {
		return nil, errdefs.Validationf("agent_id and run_id are required")
	}
	if req.Decision != oas.ApprovalDecisionRequestDecisionApproved && req.Decision != oas.ApprovalDecisionRequestDecisionRejected {
		return nil, errdefs.Validationf("decision must be 'approved' or 'rejected'")
	}

	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, req.AgentID)
	if err != nil {
		return nil, errdefs.NotFoundf("agent %q: %v", req.AgentID, err)
	}

	var snap *graph.BoardSnapshot
	if req.State.Set {
		snap = new(graph.BoardSnapshot)
		if !decodeOptJSONObject(req.State, snap) {
			snap = nil
		}
	}
	if snap == nil {
		loaded, loadErr := h.s.deps.Platform.LoadCheckpoint(ctx, req.AgentID)
		if loadErr != nil || loaded == nil {
			return nil, errdefs.Conflictf("no state provided and no checkpoint available for agent %q", req.AgentID)
		}
		snap = loaded
	}

	// Inject the verdict so the resumed `approval` script sees it via
	// board.getVar("approval_decision"). Comment is exposed alongside for
	// downstream nodes that want to surface reviewer notes.
	inputs := map[string]any{
		"approval_decision": string(req.Decision),
	}
	if comment, ok := req.Comment.Get(); ok && comment != "" {
		inputs["approval_comment"] = comment
	}

	wfReq := &workflow.Request{
		ContextID: params.ID,
		RunID:     req.RunID,
		Inputs:    inputs,
	}

	startNode := graph.RestoreBoard(snap).GetVarString(graph.VarInterruptedNode)

	if _, resumeErr := h.s.deps.Platform.ResumeAgent(ctx, agent, wfReq, snap, startNode); resumeErr != nil {
		return nil, errdefs.Internalf("resume failed: %v", resumeErr)
	}

	resp := &oas.ApprovalDecisionResponse{
		Accepted:  true,
		RunID:     req.RunID,
		Partition: oas.NewOptString("card:" + params.ID),
	}
	if h.s.deps.EventLog != nil {
		if seq, seqErr := h.s.deps.EventLog.LatestSeq(ctx); seqErr == nil {
			resp.LastSeq = oas.NewOptInt64(seq)
		}
	}
	return resp, nil
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
	worker := h.s.deps.Platform.KnowledgeWorker
	if worker == nil {
		return nil, errdefs.NotAvailablef("knowledge context worker is not configured (LLM unavailable)")
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

	// Backfill the chunk count synchronously so the first list/get after
	// ingestion reflects the true value. Processing state itself is
	// owned by the worker (it flips pending → processing → completed |
	// failed below).
	chunks := knowledge.ChunkDocument(req.Name, req.Content, knowledge.DefaultChunkConfig())
	chunkCount := len(chunks)
	if updateErr := h.s.deps.Platform.Store.UpdateDocumentStats(ctx, params.ID, doc.ID, model.DocumentStatsPatch{
		ChunkCount: &chunkCount,
	}); updateErr != nil {
		telemetry.Warn(ctx, "knowledge: update document chunk count failed",
			otellog.String("doc", doc.ID), otellog.String("error", updateErr.Error()))
	} else {
		doc.ChunkCount = chunkCount
	}

	if subErr := worker.SubmitDocument(ctx, params.ID, doc.ID, req.Name, req.Content); subErr != nil {
		// The worker already flipped the row to failed when the
		// underlying store error is the cause; surface the failure to
		// the caller so the UI can show it.
		return nil, subErr
	}
	doc.ProcessingStatus = model.ProcessingRunning

	return toOAS[oas.DatasetDocument](doc)
}

func (h *oapiHandler) DeleteDocument(ctx context.Context, params oas.DeleteDocumentParams) error {
	doc, err := h.s.deps.Platform.Store.GetDocument(ctx, params.ID, params.DocId)
	if err != nil {
		return err
	}
	// Best-effort: cancel any in-flight context generation for this doc
	// so it does not race with the delete and resurrect a "completed"
	// stats patch after the row is gone.
	if h.s.deps.Platform.KnowledgeWorker != nil {
		h.s.deps.Platform.KnowledgeWorker.Cancel(params.DocId)
	}
	if err := h.s.deps.Platform.Store.DeleteDocument(ctx, params.ID, params.DocId); err != nil {
		return err
	}
	if h.s.deps.Platform.Knowledge != nil && doc != nil {
		_ = h.s.deps.Platform.Knowledge.DeleteDocument(ctx, params.ID, doc.Name)
	}
	return nil
}

// ReprocessDocument re-submits a document to the context worker. Used
// by the UI as a manual retry path for docs that landed in failed (or
// got stuck while the worker was offline). When no worker is wired
// (no LLM configured) Reprocess returns 503 so the caller can surface
// the misconfiguration instead of looping on a dead retry.
func (h *oapiHandler) ReprocessDocument(ctx context.Context, params oas.ReprocessDocumentParams) (*oas.DatasetDocument, error) {
	worker := h.s.deps.Platform.KnowledgeWorker
	if worker == nil {
		return nil, errdefs.NotAvailablef("knowledge context worker is not configured (LLM unavailable)")
	}

	doc, err := h.s.deps.Platform.Store.GetDocument(ctx, params.ID, params.DocId)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errdefs.NotFoundf("document %s not found", params.DocId)
	}

	if err := worker.SubmitDocument(ctx, params.ID, doc.ID, doc.Name, doc.Content); err != nil {
		return nil, err
	}
	doc.ProcessingStatus = model.ProcessingRunning

	return toOAS[oas.DatasetDocument](doc)
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
		layerMap := map[oas.DatasetQueryRequestMaxLayer]knowledge.ContextLayer{
			oas.DatasetQueryRequestMaxLayerL0: knowledge.LayerAbstract,
			oas.DatasetQueryRequestMaxLayerL1: knowledge.LayerOverview,
			oas.DatasetQueryRequestMaxLayerL2: knowledge.LayerDetail,
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
		qr := oas.QueryResult{
			Content:      oas.NewOptString(res.Content),
			Score:        oas.NewOptFloat64(res.Score),
			DocumentName: oas.NewOptString(res.DocName),
			ChunkIndex:   oas.NewOptInt(res.ChunkIndex),
		}
		if layer, ok := toOASLayer(res.Layer); ok {
			qr.Layer = oas.NewOptQueryResultLayer(layer)
		}
		oasResults[i] = qr
	}
	return &oas.QueryResultList{Data: oasResults}, nil
}

// toOASLayer maps the knowledge-package ContextLayer string ("L0"/"L1"/"L2")
// onto the generated OAS enum. The second return value is false when the
// layer is unset or unrecognised so the caller can omit the field.
func toOASLayer(l knowledge.ContextLayer) (oas.QueryResultLayer, bool) {
	switch l {
	case knowledge.LayerAbstract:
		return oas.QueryResultLayerL0, true
	case knowledge.LayerOverview:
		return oas.QueryResultLayerL1, true
	case knowledge.LayerDetail:
		return oas.QueryResultLayerL2, true
	default:
		return "", false
	}
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
		h.publishAgentConfigChanged(ctx, params.ID, "version_published")
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
		h.publishAgentConfigChanged(ctx, params.ID, "version_rolled_back")
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
	configs, err := h.s.deps.Platform.Store.ListModelConfigs(ctx)
	if err != nil {
		return nil
	}
	options := []node.SelectOption{
		{Value: "", Label: "Use Default Model"},
	}
	for _, c := range configs {
		key := c.Provider + "/" + c.Model
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

func (h *oapiHandler) DeleteTemplate(ctx context.Context, params oas.DeleteTemplateParams) error {
	if h.s.deps.Platform.TemplateReg == nil {
		return errdefs.Internalf("template registry not configured")
	}
	return h.s.deps.Platform.TemplateReg.Delete(ctx, params.Name)
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

func (h *oapiHandler) UpdateSkill(ctx context.Context, params oas.UpdateSkillParams) (*oas.SkillUpdateResult, error) {
	if h.s.deps.Platform.SkillStore == nil {
		return nil, errdefs.Internalf("skill store not configured")
	}
	if err := h.s.deps.Platform.SkillStore.Update(ctx, params.Name); err != nil {
		return nil, err
	}
	return &oas.SkillUpdateResult{
		Status: oas.NewOptString("updated"),
		Name:   oas.NewOptString(params.Name),
	}, nil
}

func (h *oapiHandler) UpdateAllSkills(ctx context.Context) (*oas.SkillUpdateAllResult, error) {
	if h.s.deps.Platform.SkillStore == nil {
		return nil, errdefs.Internalf("skill store not configured")
	}
	updated, err := h.s.deps.Platform.SkillStore.UpdateAll(ctx)
	if err != nil {
		return nil, err
	}
	return &oas.SkillUpdateAllResult{Updated: updated}, nil
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

// ListModels returns only the models the user has explicitly configured
// in Settings → Add Model (model_configs table).
//
// Previously this returned llm.ListAllModels() — the full registry catalog
// of every model every provider package knows about — which surfaced
// dozens of un-configured entries in the UI.
//
// Label metadata is enriched from the provider registry when available,
// so users see e.g. "GPT-4o (OpenAI's flagship)" instead of the bare id.
func (h *oapiHandler) ListModels(ctx context.Context) (*oas.ModelList, error) {
	configs, err := h.s.deps.Platform.Store.ListModelConfigs(ctx)
	if err != nil {
		return nil, err
	}
	defaultProvider, defaultModel := h.resolveDefaultModel(ctx)

	labels := make(map[string]string)
	for _, m := range llm.ListAllModels() {
		labels[m.Provider+"/"+m.Name] = m.Label
	}

	items := make([]oas.ModelInfo, 0, len(configs))
	for _, c := range configs {
		label := labels[c.Provider+"/"+c.Model]
		if label == "" {
			label = c.Model
		}
		items = append(items, oas.ModelInfo{
			Provider:  oas.NewOptString(c.Provider),
			Model:     oas.NewOptString(c.Model),
			Label:     oas.NewOptString(label),
			IsDefault: oas.NewOptBool(c.Provider == defaultProvider && c.Model == defaultModel),
		})
	}
	return &oas.ModelList{Data: items}, nil
}

// AddModel registers a per-model entry surfaced in Settings →
// Configured Models. When api_key / base_url are supplied (i.e. the
// provider has not yet been configured) they are persisted to
// provider_configs so the resolver can instantiate the model.
//
// The optional `extra` request field is split into:
//   - `extra.caps`  → ModelConfig.Caps   (typed; the resolver applies these)
//   - the remainder → ModelConfig.Extra  (shallow-merged onto provider config)
//
// This mirrors the SDK's typed ModelConfig surface and removes the
// dead-link bug where caps stuffed into untyped maps were silently
// dropped by the resolver.
func (h *oapiHandler) AddModel(ctx context.Context, req *oas.AddModelRequest) (*oas.ModelInfo, error) {
	if req.Provider == "" || req.Model == "" {
		return nil, errdefs.Validationf("provider and model are required")
	}

	apiKey, _ := req.APIKey.Get()
	baseURL, _ := req.BaseURL.Get()
	if apiKey != "" || baseURL != "" {
		// Merge into existing provider credentials rather than
		// replacing wholesale: adding a second model for an already-
		// configured provider frequently omits api_key (the UI doesn't
		// force a retype), and a blind SetProviderConfig would wipe
		// the existing key, breaking every other model under that
		// provider. Only fields the caller explicitly supplied get
		// overwritten.
		credCfg := map[string]any{}
		if existing, err := h.s.deps.Platform.Store.GetProviderConfig(ctx, req.Provider); err == nil && existing != nil {
			for k, v := range existing.Config {
				credCfg[k] = v
			}
		}
		if apiKey != "" {
			credCfg["api_key"] = apiKey
		}
		if baseURL != "" {
			credCfg["base_url"] = baseURL
		}
		creds := &llm.ProviderConfig{Provider: req.Provider, Config: credCfg}
		if err := h.s.deps.Platform.Store.SetProviderConfig(ctx, creds); err != nil {
			return nil, err
		}
	}

	mc := &model.ModelConfig{Provider: req.Provider, Model: req.Model}
	var extra map[string]any
	if decodeOptJSONObject(req.Extra, &extra) {
		if rawCaps, ok := extra["caps"].(map[string]any); ok {
			mc.Caps = capsFromMap(rawCaps)
			delete(extra, "caps")
		}
		if len(extra) > 0 {
			mc.Extra = extra
		}
	}
	if err := h.s.deps.Platform.Store.SetModelConfig(ctx, mc); err != nil {
		return nil, err
	}

	if h.s.deps.Platform.LLMResolver != nil {
		h.s.deps.Platform.LLMResolver.InvalidateCache(req.Provider)
	}

	return &oas.ModelInfo{
		Provider: oas.NewOptString(req.Provider),
		Model:    oas.NewOptString(req.Model),
	}, nil
}

// capsFromMap parses the UI's caps payload into typed llm.ModelCaps.
// Accepts both the structured form `{"disabled":{"temperature":true}}`
// and the flat form `{"no_temperature":true, "no_json_mode":true}`,
// matching the SDK's capsFromConfig fallback chain so users can paste
// whichever form they have at hand.
func capsFromMap(m map[string]any) llm.ModelCaps {
	if disabled, ok := m["disabled"].(map[string]any); ok {
		caps := llm.ModelCaps{Disabled: make(map[llm.Capability]bool)}
		for k, v := range disabled {
			if b, _ := v.(bool); b {
				caps.Disabled[llm.Capability(k)] = true
			}
		}
		if len(caps.Disabled) == 0 {
			return llm.ModelCaps{}
		}
		return caps
	}
	flat := map[string]llm.Capability{
		"no_temperature": llm.CapTemperature,
		"no_json_schema": llm.CapJSONSchema,
		"no_json_mode":   llm.CapJSONMode,
	}
	caps := llm.ModelCaps{Disabled: make(map[llm.Capability]bool)}
	for k, c := range flat {
		if b, _ := m[k].(bool); b {
			caps.Disabled[c] = true
		}
	}
	if len(caps.Disabled) == 0 {
		return llm.ModelCaps{}
	}
	return caps
}

// SetDefaultModel marks {provider, model} as the resolver's default.
// The pair must already be a configured per-model row (created via
// AddModel); otherwise the resolver would point at a non-existent
// entry.
func (h *oapiHandler) SetDefaultModel(ctx context.Context, req *oas.SetDefaultModelRequest) error {
	if req.Provider == "" || req.Model == "" {
		return errdefs.Validationf("provider and model are required")
	}
	if _, err := h.s.deps.Platform.Store.GetModelConfig(ctx, req.Provider, req.Model); err != nil {
		return errdefs.Validationf("model %s/%s is not configured; add it first", req.Provider, req.Model)
	}
	if err := h.s.deps.Platform.Store.SetDefaultModel(ctx,
		&model.DefaultModelRef{Provider: req.Provider, Model: req.Model}); err != nil {
		return err
	}
	if h.s.deps.Platform.LLMResolver != nil {
		h.s.deps.Platform.LLMResolver.InvalidateCache("")
	}
	return nil
}

// DeleteModel removes the per-model row identified by "<provider>/<model>".
// When the deleted model was the current default the default pointer is
// cleared too, so the next Resolve call falls back to the resolver's
// fallback model rather than dangling.
func (h *oapiHandler) DeleteModel(ctx context.Context, params oas.DeleteModelParams) error {
	provider, modelName := splitProviderModelPath(params.ModelID)
	if provider == "" || modelName == "" {
		return errdefs.Validationf("model id must be in the form '<provider>/<model>'")
	}
	if err := h.s.deps.Platform.Store.DeleteModelConfig(ctx, provider, modelName); err != nil {
		return err
	}
	if ref, err := h.s.deps.Platform.Store.GetDefaultModel(ctx); err == nil && ref != nil &&
		ref.Provider == provider && ref.Model == modelName {
		_ = h.s.deps.Platform.Store.ClearDefaultModel(ctx)
	}
	if h.s.deps.Platform.LLMResolver != nil {
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

// ListProviders returns the catalog of providers known to the LLM
// registry, each annotated with whether the user has supplied
// provider-level credentials. After migration 005 provider_configs
// holds only plain credential rows, so no PK-prefix filtering is
// necessary.
func (h *oapiHandler) ListProviders(ctx context.Context) (*oas.ProviderList, error) {
	providers := llm.ListProviders()
	allModels := llm.ListAllModels()
	configured := make(map[string]bool)
	configs, err := h.s.deps.Platform.Store.ListProviderConfigs(ctx)
	if err == nil {
		for _, pc := range configs {
			configured[pc.Provider] = true
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
	var extra map[string]any
	if decodeOptJSONObject(req.Extra, &extra) {
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
	ref, err := h.s.deps.Platform.Store.GetDefaultModel(ctx)
	if err != nil || ref == nil {
		return "", ""
	}
	return ref.Provider, ref.Model
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
	data := jsonObjectsFromAny(points)
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

func pluginInfoToOAS(info plugin.PluginInfo) oas.PluginInfo {
	out := oas.PluginInfo{
		ID:      info.ID,
		Name:    info.Name,
		Builtin: info.Builtin,
	}
	if info.Version != "" {
		out.Version = oas.NewOptString(info.Version)
	}
	if info.Type != "" {
		out.Type = oas.NewOptString(string(info.Type))
	}
	if info.Description != "" {
		out.Description = oas.NewOptString(info.Description)
	}
	if info.Author != "" {
		out.Author = oas.NewOptString(info.Author)
	}
	if info.Icon != "" {
		out.Icon = oas.NewOptString(info.Icon)
	}
	if info.Homepage != "" {
		out.Homepage = oas.NewOptString(info.Homepage)
	}
	if !info.CreatedAt.IsZero() {
		out.CreatedAt = oas.NewOptDateTime(info.CreatedAt)
	}
	return out
}

func installedPluginToOAS(p plugin.InstalledPlugin) oas.PluginDetail {
	d := oas.PluginDetail{
		Info:   pluginInfoToOAS(p.Info),
		Status: oas.PluginDetailStatus(p.Status),
	}
	if len(p.Config) > 0 {
		d.Config = oas.NewOptJSONObject(toJSONObject(p.Config))
	}
	if p.Error != "" {
		d.Error = oas.NewOptString(p.Error)
	}
	return d
}

func (h *oapiHandler) ListPlugins(ctx context.Context, params oas.ListPluginsParams) (*oas.PluginList, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return &oas.PluginList{Data: []oas.PluginDetail{}}, nil
	}
	all := h.s.deps.Platform.PluginReg.List()
	filterType, hasFilter := params.Type.Get()
	items := make([]oas.PluginDetail, 0, len(all))
	for _, p := range all {
		if hasFilter && filterType != "" && string(p.Info.Type) != filterType {
			continue
		}
		items = append(items, installedPluginToOAS(p))
	}
	return &oas.PluginList{Data: items}, nil
}

func (h *oapiHandler) GetPlugin(ctx context.Context, params oas.GetPluginParams) (*oas.PluginDetail, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	all := h.s.deps.Platform.PluginReg.List()
	for _, p := range all {
		if p.Info.ID == params.Name {
			d := installedPluginToOAS(p)
			return &d, nil
		}
	}
	return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
}

func (h *oapiHandler) resolvePluginInfo(name string) oas.PluginInfo {
	if h.s.deps.Platform.PluginReg != nil {
		if p, ok := h.s.deps.Platform.PluginReg.Get(name); ok {
			return pluginInfoToOAS(p.Info())
		}
	}
	return oas.PluginInfo{ID: name, Name: name}
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
	info := h.resolvePluginInfo(params.Name)
	return &info, nil
}

func (h *oapiHandler) DisablePlugin(ctx context.Context, params oas.DisablePluginParams) (*oas.PluginInfo, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	if err := h.s.deps.Platform.PluginReg.Disable(ctx, params.Name); err != nil {
		return nil, err
	}
	info := h.resolvePluginInfo(params.Name)
	return &info, nil
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
	info := h.resolvePluginInfo(params.Name)
	return &info, nil
}

func (h *oapiHandler) ReloadPlugins(ctx context.Context) (*oas.PluginReloadResult, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return &oas.PluginReloadResult{Added: []string{}, Removed: []string{}}, nil
	}
	added, removed, err := h.s.deps.Platform.PluginReg.Reload(ctx)
	if err != nil {
		return nil, err
	}
	h.s.deps.Platform.SyncPluginSchemas()
	return &oas.PluginReloadResult{Added: added, Removed: removed}, nil
}

func (h *oapiHandler) UploadPlugin(ctx context.Context, req *oas.UploadPluginReq) (*oas.PluginUploadResult, error) {
	if h.s.deps.Platform.PluginReg == nil {
		return nil, errdefs.Forbiddenf("plugin system is not configured")
	}
	if req == nil {
		return nil, errdefs.Validationf("missing upload body")
	}
	filename := strings.TrimSpace(req.File.Name)
	if filename == "" {
		return nil, errdefs.Validationf("upload filename is required")
	}
	added, removed, size, err := h.s.deps.Platform.PluginReg.InstallBinary(ctx, filename, req.File.File)
	if err != nil {
		return nil, err
	}
	h.s.deps.Platform.SyncPluginSchemas()
	result := &oas.PluginUploadResult{
		Name:    oas.NewOptString(filepath.Base(filename)),
		Size:    oas.NewOptInt(int(size)),
		Added:   added,
		Removed: removed,
	}
	if len(added) > 0 {
		result.ID = oas.NewOptString(added[0])
	}
	return result, nil
}

func (h *oapiHandler) DeletePlugin(ctx context.Context, params oas.DeletePluginParams) error {
	if h.s.deps.Platform.PluginReg == nil {
		return errdefs.NotFoundf("plugin %s not found", params.Name)
	}
	if err := h.s.deps.Platform.PluginReg.RemoveBinary(ctx, params.Name); err != nil {
		return err
	}
	h.s.deps.Platform.SyncPluginSchemas()
	return nil
}

// ══════════════════════════ Kanban ══════════════════════════

func lookupRuntimeBoard(plat *platform.Platform) (*kanban.TaskBoard, bool) {
	board := plat.TaskBoard()
	return board, board != nil
}

// currentRealmID returns the realm/runtime ID this process is currently
// serving (SingleRealmProvider). Used to derive the partition cursor and
// per-card realm_id in /kanban/cards. Empty string when no realm has been
// resolved yet — callers should treat that as "no snapshot available".
func (h *oapiHandler) currentRealmID() string {
	if h.s.deps.Platform == nil || h.s.deps.Platform.Realms == nil {
		return ""
	}
	r, ok := h.s.deps.Platform.Realms.Current()
	if !ok || r == nil {
		return ""
	}
	return r.ID()
}

// GetKanbanCards returns the initial snapshot consumed by the frontend
// Kanban view. After R5 (§7.1.5 / §13) the response carries:
//
//   - last_seq / last_event_ts: cursor for the envelope subscription that
//     drives the steady-state board (clients subscribe to
//     `partition=runtime:<realm_id>` with `since=last_seq`);
//   - realm_id (top-level + per card): so single-board UIs do not need to
//     peek inside `data` to know which runtime they're looking at.
//
// The legacy `events` array and `lastUpdate` field are gone — the canonical
// stream of card mutations lives in the event log, not in this snapshot.
func (h *oapiHandler) GetKanbanCards(ctx context.Context) (*oas.KanbanCardList, error) {
	realmID := h.currentRealmID()
	resp := &oas.KanbanCardList{Data: []oas.KanbanCardSummary{}}
	if realmID != "" {
		resp.RealmID = oas.NewOptString(realmID)
	}
	board, ok := lookupRuntimeBoard(h.s.deps.Platform)
	if ok {
		cards := board.Cards()
		resp.Data = make([]oas.KanbanCardSummary, 0, len(cards))
		for _, c := range cards {
			resp.Data = append(resp.Data, kanbanCardSummary(c, realmID))
		}
	}
	if h.s.deps.EventLog != nil && realmID != "" {
		seq, ts, err := h.s.deps.EventLog.LatestInPartition(ctx, eventlog.PartitionRuntime(realmID))
		if err == nil {
			resp.LastSeq = oas.NewOptInt64(seq)
			if !ts.IsZero() {
				resp.LastEventTs = oas.NewOptDateTime(ts)
			}
		}
	}
	return resp, nil
}

func kanbanCardSummary(c kanban.CardInfo, realmID string) oas.KanbanCardSummary {
	out := oas.KanbanCardSummary{
		ID:        c.ID,
		Type:      c.Type,
		Status:    c.Status,
		Producer:  c.Producer,
		Consumer:  c.Consumer,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
	if c.Query != "" {
		out.Query = oas.NewOptString(c.Query)
	}
	if c.TargetAgentID != "" {
		out.TargetAgentID = oas.NewOptString(c.TargetAgentID)
	}
	if c.Output != "" {
		out.Output = oas.NewOptString(c.Output)
	}
	if c.Error != "" {
		out.Error = oas.NewOptString(c.Error)
	}
	if c.RunID != "" {
		out.RunID = oas.NewOptString(c.RunID)
	}
	if c.ElapsedMs != 0 {
		out.ElapsedMs = oas.NewOptInt64(c.ElapsedMs)
	}
	if len(c.Meta) > 0 {
		meta := make(oas.KanbanCardSummaryMeta, len(c.Meta))
		for k, v := range c.Meta {
			meta[k] = v
		}
		out.Meta = oas.NewOptKanbanCardSummaryMeta(meta)
	}
	if realmID != "" {
		out.RealmID = oas.NewOptString(realmID)
	}
	return out
}

// GetKanbanTimeline returns the per-card timeline. The schema is unchanged
// (R5 §13 only requires the response to be derived from envelopes, not a
// breaking field rename) — sdk/kanban.Board.Timeline() already projects
// the same view from the in-memory card list, which is itself driven by
// the event log via the kanban bridge. So we keep the existing pass-through.
func (h *oapiHandler) GetKanbanTimeline(ctx context.Context) (*oas.KanbanTimeline, error) {
	if board, ok := lookupRuntimeBoard(h.s.deps.Platform); ok {
		return &oas.KanbanTimeline{Data: jsonObjectsFromAny(board.Timeline())}, nil
	}
	return &oas.KanbanTimeline{Data: []oas.JSONObject{}}, nil
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
		Nodes: []oas.JSONObject{},
		Edges: []oas.JSONObject{},
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

// ══════════════════════════ Chat command ══════════════════════════

// StartConversationRun is the command-side replacement of the deprecated
// `POST /chat/stream` SSE endpoint. It accepts a chat run, fires it
// asynchronously via the realm runtime, and returns the identifiers the
// caller should use to subscribe to the resulting envelope stream
// (`GET /api/events?partition=card:{id}&since=last_seq`).
func (h *oapiHandler) StartConversationRun(ctx context.Context, req *oas.ChatRequest, params oas.StartConversationRunParams) (*oas.ChatStartResponse, error) {
	if req == nil {
		return nil, errdefs.Validationf("chat request body required")
	}
	if req.AgentID == "" {
		return nil, errdefs.Validationf("agent_id is required")
	}
	if req.Query == "" {
		return nil, errdefs.Validationf("query is required")
	}

	agent, err := h.s.deps.Platform.Store.GetAgent(ctx, req.AgentID)
	if err != nil {
		return nil, errdefs.NotFoundf("agent %q: %v", req.AgentID, err)
	}

	convID := params.ID
	if convID == "" {
		convID = ownerRealmID + "--" + agent.AgentID
	}

	var inputs map[string]any
	decodeOptJSONObject(req.Inputs, &inputs)
	if agent.Type == model.AgentTypeCoPilot {
		httpReq, _ := HTTPRequestFromContext(ctx)
		if httpReq != nil {
			compat := chatRequestCompat{
				AgentID:        req.AgentID,
				ConversationID: convID,
				Query:          req.Query,
				Inputs:         inputs,
			}
			if async, ok := req.Async.Get(); ok {
				compat.Async = async
			}
			inputs = h.s.buildCoPilotInputsFromChat(httpReq, agent, compat)
		}
	}

	runID := newRunID()
	wfReq := &workflow.Request{
		ContextID: convID,
		RuntimeID: ownerRealmID,
		RunID:     runID,
		Message:   sdkmodel.NewTextMessage(sdkmodel.RoleUser, req.Query),
		Inputs:    inputs,
	}

	persistent := agent.Type == model.AgentTypeCoPilot
	doneCh, sub, _, runErr := h.s.deps.Platform.RunAgentStreaming(ctx, agent, wfReq, persistent)
	if runErr != nil {
		return nil, errdefs.Internalf("start run: %v", runErr)
	}
	// RunAgentStreaming hands us a bus subscription that we intentionally
	// don't read from: post-Phase 9 every observable signal
	// (agent.stream.delta, kanban.card.*, ...) is published to the event
	// log and reaches the client through the unified envelope stream.
	// We still have to drain `sub` until the run finishes, otherwise the
	// actor's outbound channel back-pressures and stalls the run.
	go func() {
		defer func() { _ = sub.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sub.Events():
				if !ok {
					return
				}
			case <-doneCh:
				return
			}
		}
	}()

	resp := &oas.ChatStartResponse{
		Accepted:       true,
		ConversationID: convID,
		RunID:          runID,
		Partition:      "card:" + convID,
	}
	if h.s.deps.EventLog != nil {
		if seq, seqErr := h.s.deps.EventLog.LatestSeq(ctx); seqErr == nil {
			resp.LastSeq = oas.NewOptInt64(seq)
		}
	}
	return resp, nil
}

// newRunID returns a 32-char hex identifier used to correlate runtime events
// (agent.stream.delta, kanban.card.*) with the run that produced them.
func newRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ══════════════════════════ WS Ticket ══════════════════════════

// CreateWSTicket issues a single-use ticket bound to (partition, since)
// per §12.3. The ticket is consumed by /api/events/ws on the first
// subscribe; subsequent subscribes on the same connection still go
// through the actor's policy (so leaked tickets can't escalate scope).
func (h *oapiHandler) CreateWSTicket(ctx context.Context, req *oas.WSTicketRequest) (*oas.WSTicketResponse, error) {
	if req == nil || req.Partition == "" {
		return nil, errdefs.Validationf("partition required")
	}
	if _, _, ok := eventlog.SplitPartition(req.Partition); !ok {
		return nil, errdefs.Validationf("partition %q malformed", req.Partition)
	}
	if req.Since < 0 {
		return nil, errdefs.Validationf("since must be >= 0")
	}
	actor, _ := policy.ActorFrom(ctx)
	// Policy is always wired by bootstrap.wireHTTP (OwnerOnly in
	// production, an explicit allow-all in apitest), so we can fail
	// hard rather than silently bypass when it would be nil — that
	// nil branch was the §11.1 "subscribe must go through Policy"
	// hole pre-Phase 9.
	dec, err := h.s.deps.Policy.AllowSubscribe(ctx, actor, policy.SubscribeOptions{
		Partitions: []string{req.Partition},
	})
	if err != nil {
		return nil, err
	}
	if !dec.Allow {
		return nil, errdefs.Forbiddenf("%s", dec.Reason)
	}
	ticket, expiresAt, err := h.s.wsTickets.issue(wsTicketTTL, actor, req.Partition, req.Since)
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

// publishAgentConfigChanged broadcasts an agent config change on two
// channels:
//
//  1. sdk/event bus — kept for in-process consumers (cron scheduler reload,
//     graph hot-reload in IDE-side panels) that pre-date the event log.
//  2. eventlog envelope `agent.config.changed` — the canonical persisted
//     fact, replayable and visible to every transport. This is what the
//     frontend AgentDetailLayout subscribes to via the unified envelope
//     stream (GET /api/events / /api/events/stream / /api/events/ws),
//     replacing the bespoke websocket polling that pre-dated §12.
//
// runtime_id is required by the envelope partition; we resolve it from
// the current realm (single-realm provider). When no realm is resolved
// yet (boot race), the envelope publish is skipped — bus publish still
// goes out so in-process listeners see the change.
func (h *oapiHandler) publishAgentConfigChanged(ctx context.Context, agentID, change string) {
	if h.s.deps.Platform.EventBus != nil {
		_ = h.s.deps.Platform.EventBus.Publish(ctx, event.Event{
			Type:    event.EventAgentConfigChanged,
			ActorID: agentID,
		})
	}
	runtimeID := h.currentRealmID()
	if runtimeID == "" || h.s.deps.EventLog == nil {
		return
	}
	if change == "" {
		change = "updated"
	}
	_, _ = eventlog.PublishAgentConfigChanged(ctx, h.s.deps.EventLog, runtimeID, eventlog.AgentConfigChangedPayload{
		AgentID:   agentID,
		RuntimeID: runtimeID,
		Change:    change,
	})
}
