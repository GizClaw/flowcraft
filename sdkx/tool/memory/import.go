package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// ImportToolKind is the canonical tool name for the memory
// Import tool. Hosts register the tool under this name; the
// deploy document's tools: list references it the same way any
// other tool is referenced.
const ImportToolKind = "memory.import"

// ScopeConfig is the YAML-friendly form of a memory.Scope the
// import tool carries. Only the hard-partition fields are
// part of the schema; soft fields (AgentID, ConversationID,
// DatasetID) are request-time concerns, not deployment-time.
type ScopeConfig struct {
	RuntimeID string `yaml:"runtime_id"`
	UserID    string `yaml:"user_id,omitempty"`
}

// resolveScope returns a fully-populated memory.Scope,
// filling any empty field from rt.Spec().DefaultScope. The
// kernel rejects an empty RuntimeID, so the resolved scope is
// always valid when rt is built by the config package.
func resolveScope(rt *memory.Runtime, cfg ScopeConfig) memory.Scope {
	def := rt.Spec().DefaultScope
	if cfg.RuntimeID == "" {
		cfg.RuntimeID = def.RuntimeID
	}
	if cfg.UserID == "" {
		cfg.UserID = def.UserID
	}
	return memory.Scope{
		RuntimeID: cfg.RuntimeID,
		UserID:    cfg.UserID,
	}
}

// ImportSettings is the configuration the host passes to
// NewImportTool. The fields map directly to ImportRequest, with
// the addition of Scope which is resolved from
// runtime.Spec().DefaultScope at construction time.
type ImportSettings struct {
	// Scope is the memory scope the Import runs against.
	// Empty fields fall back to runtime.Spec().DefaultScope.
	Scope ScopeConfig
	// DatasetID is the soft partition for the document
	// collection. Required — empty is a build error.
	DatasetID string
	// Source is the default document locator. The
	// tool's JSON arguments may override it on a per-call
	// basis; an empty Source AND empty arguments is a
	// runtime validation error.
	Source string
	// Tags are opaque annotations attached to every
	// imported chunk. The tool's JSON arguments may
	// override these on a per-call basis; an empty Tags
	// AND empty arguments.tags means "no tags".
	Tags []string
	// ChunkPolicy controls how the source is split. A
	// zero ChunkPolicy means "use the runtime default";
	// the tool's JSON arguments may override on a
	// per-call basis.
	ChunkPolicy memory.ChunkPolicy
}

// Validate enforces the documented invariants. Called by
// NewImportTool and by tests that build a tool without
// registration.
func (s ImportSettings) Validate() error {
	if s.DatasetID == "" {
		return errdefs.Validation(fmt.Errorf(
			"memory tool: %s requires dataset_id", ImportToolKind))
	}
	return nil
}

// ImportTool is a tool.Tool that wraps a *memory.Runtime's
// Import op. The runtime is shared with the rest of the agent;
// the tool only forwards an ImportRequest and renders the
// ImportResponse as JSON. It is safe for concurrent use.
type ImportTool struct {
	rt    *memory.Runtime
	scope memory.Scope
	cfg   ImportSettings
	def   message.Definition
}

// NewImportTool validates settings and returns a memory.import
// tool. The scope is resolved from rt.Spec().DefaultScope at
// construction time, so subsequent changes to the runtime's
// default scope do not affect this tool.
func NewImportTool(rt *memory.Runtime, settings ImportSettings) (*ImportTool, error) {
	if rt == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory tool: %s: runtime is nil", ImportToolKind))
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	def, err := importDefinition()
	if err != nil {
		return nil, err
	}
	return &ImportTool{
		rt:    rt,
		scope: resolveScope(rt, settings.Scope),
		cfg:   settings,
		def:   def,
	}, nil
}

// Definition returns the tool's schema. The LLM sees this when
// deciding whether to call the tool. The InputSchema is built
// once at construction and cached.
func (t *ImportTool) Definition() message.Definition { return t.def }

// Execute performs an Import against the bound runtime. The
// arguments JSON object may override any field of
// ImportSettings on a per-call basis; an omitted field falls
// back to the tool's configured default.
func (t *ImportTool) Execute(ctx context.Context, args string) (string, error) {
	var parsed struct {
		Source      string             `json:"source,omitempty"`
		DatasetID   string             `json:"dataset_id,omitempty"`
		Tags        []string           `json:"tags,omitempty"`
		ChunkPolicy memory.ChunkPolicy `json:"chunk_policy,omitempty"`
	}
	if args != "" {
		trimmed := strings.TrimSpace(args)
		if trimmed == "" || trimmed[0] != '{' {
			return "", errdefs.Validation(fmt.Errorf(
				"memory tool: %s: arguments must be a JSON object", ImportToolKind))
		}
		decoder := json.NewDecoder(bytes.NewBufferString(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&parsed); err != nil {
			return "", errdefs.Validation(fmt.Errorf(
				"memory tool: %s: parse arguments: %w", ImportToolKind, err))
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return "", errdefs.Validation(fmt.Errorf(
				"memory tool: %s: parse arguments: %w", ImportToolKind, err))
		}
	}
	source := t.cfg.Source
	if parsed.Source != "" {
		source = parsed.Source
	}
	dataset := t.cfg.DatasetID
	if parsed.DatasetID != "" {
		dataset = parsed.DatasetID
	}
	tags := t.cfg.Tags
	if parsed.Tags != nil {
		tags = parsed.Tags
	}
	policy := t.cfg.ChunkPolicy
	if !parsed.ChunkPolicy.IsZero() {
		policy = parsed.ChunkPolicy
	}
	if source == "" {
		return "", errdefs.Validation(fmt.Errorf(
			"memory tool: %s: source is required (no default and no override in arguments)",
			ImportToolKind))
	}
	req := memory.ImportRequest{
		Scope:       t.scope,
		DatasetID:   dataset,
		Source:      source,
		Tags:        tags,
		ChunkPolicy: policy,
	}
	resp, err := t.rt.ExecuteImport(ctx, req)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]any{
		"document_id": resp.DocumentID,
		"chunk_count": resp.ChunkCount,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON value")
	}
	return err
}

// RegisterImportTool is a host-side convenience: it constructs
// and registers a memory.import tool against the given
// registry. Returns the registered *ImportTool so callers can
// adjust metadata or post-register configuration.
func RegisterImportTool(reg *tool.Registry, rt *memory.Runtime, settings ImportSettings) (*ImportTool, error) {
	if reg == nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"memory tool: %s: registry is nil", ImportToolKind))
	}
	t, err := NewImportTool(rt, settings)
	if err != nil {
		return nil, err
	}
	reg.Register(t)
	return t, nil
}

// importDefinition builds the JSON-Schema describing the
// tool's arguments. The schema is intentionally permissive:
// all fields are optional because the tool's settings supply
// defaults the LLM may override.
func importDefinition() (message.Definition, error) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{
				"type":        "string",
				"description": "Document locator (path, URI, …). Falls back to the tool's configured source when omitted.",
			},
			"dataset_id": map[string]any{
				"type":        "string",
				"description": "Dataset partition id. Falls back to the tool's configured dataset_id when omitted.",
			},
			"tags": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Tags to attach to every imported chunk. Overrides the tool's configured tags when present.",
			},
			"chunk_policy": map[string]any{
				"type":        "object",
				"description": "Optional chunking policy override. Fields: target, min_chunk_size, max_chunk_size, tokenizer, overlap, splitter, respect_code.",
			},
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return message.Definition{}, err
	}
	return message.Definition{
		Name:        ImportToolKind,
		Description: "Import a document into the memory knowledge store. Returns the new document id and chunk count.",
		InputSchema: raw,
	}, nil
}
