package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/GizClaw/flowcraft/internal/model"
)

var refPattern = regexp.MustCompile(`\[ref:(node|agent):([^\]]+)\]`)

type chatRequestCompat struct {
	AgentID        string         `json:"agent_id"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Query          string         `json:"query"`
	Inputs         map[string]any `json:"inputs,omitempty"`
	Async          bool           `json:"async,omitempty"`
}

type CoPilotRequest struct {
	Query   string         `json:"query"`
	AgentID string         `json:"agent_id,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

func (s *Server) buildCoPilotInputs(r *http.Request, req CoPilotRequest) map[string]any {
	inputs := make(map[string]any)
	for k, v := range req.Context {
		inputs[k] = v
	}
	var targetAgent *model.Agent
	if req.AgentID != "" {
		a, err := s.deps.Platform.Store.GetAgent(r.Context(), req.AgentID)
		if err == nil {
			targetAgent = a
			inputs["current_agent_id"] = a.ID
			inputs["current_agent_name"] = a.Name
		if gd := a.StrategyDef.AsGraph(); gd != nil {
			inputs["current_graph_summary"] = summarizeGraph(gd)
		}
		}
	}
	refContext := s.parseRefMarkers(r.Context(), req.Query, targetAgent)
	if len(refContext) > 0 {
		refData, _ := json.Marshal(refContext)
		inputs["ref_context"] = string(refData)
	}
	return inputs
}

func (s *Server) parseRefMarkers(ctx context.Context, query string, targetAgent *model.Agent) map[string]any {
	matches := refPattern.FindAllStringSubmatch(query, -1)
	if len(matches) == 0 {
		return nil
	}
	refContext := make(map[string]any, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		refType, refID := match[1], match[2]
		switch refType {
		case "node":
			if info := findNodeInAgent(targetAgent, refID); info != nil {
				refContext[refID] = info
				continue
			}
			refContext[refID] = map[string]string{"type": "node", "id": refID, "status": "not_found"}
		case "agent":
			a, err := s.deps.Platform.Store.GetAgent(ctx, refID)
			if err != nil {
				refContext[refID] = map[string]string{"type": "agent", "id": refID, "status": "not_found"}
				continue
			}
			refContext[refID] = map[string]any{"type": "agent", "id": a.ID, "name": a.Name, "summary": summarizeGraph(a.StrategyDef.AsGraph())}
		}
	}
	return refContext
}

func findNodeInAgent(a *model.Agent, nodeID string) map[string]any {
	if a == nil {
		return nil
	}
	gd := a.StrategyDef.AsGraph()
	if gd == nil {
		return nil
	}
	for _, node := range gd.Nodes {
		if node.ID == nodeID {
			return map[string]any{
				"type": "node", "id": node.ID, "node_type": node.Type,
				"config": node.Config, "agent_id": a.ID, "agent_name": a.Name,
			}
		}
	}
	return nil
}

func summarizeGraph(def *model.GraphDefinition) string {
	type nodeBrief struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	nodes := make([]nodeBrief, len(def.Nodes))
	for i, n := range def.Nodes {
		nodes[i] = nodeBrief{ID: n.ID, Type: n.Type}
	}
	data, _ := json.Marshal(map[string]any{
		"name": def.Name, "entry": def.Entry,
		"node_count": len(def.Nodes), "edge_count": len(def.Edges), "nodes": nodes,
	})
	return string(data)
}

func (s *Server) buildCoPilotInputsFromChat(r *http.Request, a *model.Agent, req chatRequestCompat) map[string]any {
	inputs := make(map[string]any, len(req.Inputs))
	for k, v := range req.Inputs {
		inputs[k] = v
	}
	targetAgentID := ""
	if req.Inputs != nil {
		if raw, ok := req.Inputs["agent_id"].(string); ok {
			targetAgentID = raw
		}
	}
	if raw, ok := inputs["copilot_context"].(map[string]any); ok {
		delete(inputs, "copilot_context")
		if nestedTargetAgentID, ok := raw["current_agent_id"].(string); ok && nestedTargetAgentID != "" {
			targetAgentID = nestedTargetAgentID
		}
		if refs, ok := raw["refs"]; ok {
			inputs["refs"] = refs
		}
		if graphContext, ok := raw["graph_context"]; ok {
			inputs["graph_context"] = graphContext
		}
	}
	copilotReq := CoPilotRequest{Query: req.Query, AgentID: targetAgentID, Context: inputs}
	return s.buildCoPilotInputs(r, copilotReq)
}
