package claw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

const workspaceStateVar = "workspace_state"

type contextState struct {
	ContextID string         `json:"context_id"`
	UpdatedAt time.Time      `json:"updated_at"`
	LastRunID string         `json:"last_run_id,omitempty"`
	Vars      map[string]any `json:"vars,omitempty"`
}

func (c *Claw) loadContextState(ctx context.Context, id string) (contextState, error) {
	var st contextState
	raw, err := c.stateWorkspace().Read(ctx, contextStatePath(id))
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			return contextState{ContextID: id, Vars: map[string]any{}}, nil
		}
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	if st.Vars == nil {
		st.Vars = map[string]any{}
	}
	return st, nil
}

func (c *Claw) saveContextState(ctx context.Context, id string, result *agent.Result, inputVars ...map[string]any) error {
	if result == nil || result.LastBoard == nil {
		return nil
	}
	vars := map[string]any{}
	for _, input := range inputVars {
		for k, v := range persistentVars(input) {
			vars[k] = v
		}
	}
	for k, v := range persistentVars(result.LastBoard.Vars()) {
		vars[k] = v
	}
	st := contextState{
		ContextID: id,
		UpdatedAt: time.Now(),
		LastRunID: result.RunID,
		Vars:      vars,
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	ws := c.stateWorkspace()
	path := contextStatePath(id)
	tmp := path + ".tmp"
	if err := ws.Write(ctx, tmp, raw); err != nil {
		return err
	}
	return ws.Rename(ctx, tmp, path)
}

func (c *Claw) stateWorkspace() sdkworkspace.Workspace {
	return sdkworkspace.Sub(c.ws, c.cfg.Workspace.StateRoot)
}

func (c *Claw) contextID() string {
	if c == nil {
		return defaultConversationContextID
	}
	id := strings.TrimSpace(c.cfg.Conversation.ContextID)
	if id == "" {
		return defaultConversationContextID
	}
	return id
}

func contextStatePath(id string) string {
	if strings.TrimSpace(id) == "" {
		id = defaultConversationContextID
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(id))
	return filepath.ToSlash(filepath.Join("contexts", encoded+".json"))
}

func persistentVars(vars map[string]any) map[string]any {
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if skipPersistentVar(k) {
			continue
		}
		var roundTrip any
		raw, err := json.Marshal(v)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(raw, &roundTrip); err != nil {
			continue
		}
		out[k] = roundTrip
	}
	return out
}

func skipPersistentVar(key string) bool {
	switch key {
	case "", workspaceStateVar, "response", "usage", "tool_pending", "tool_output",
		"agent_steps", "__usage", "__prev_message_count", "__summary_index":
		return true
	}
	return strings.HasPrefix(key, "__") || strings.HasPrefix(key, "tmp_")
}
