package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/host"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/test/internal/stack"
)

// countingApprover is the deployment's approver. The test's point is that it is never asked: a
// delegated child's command goes to the parent that opened it, not to a person.
type countingApprover struct{ asked []host.ApprovalRequest }

func (a *countingApprover) Approve(_ context.Context, request host.ApprovalRequest) error {
	a.asked = append(a.asked, request)
	return nil
}

// TestADelegatedChildAsksItsParentBeforeItRuns is the whole chain: a parent delegates, the child's
// command raises an approval question, the question reaches the parent's model, the parent answers,
// and the command runs — exactly once, because the suspension happened before anything executed.
//
// The replies are consumed in call order across both sessions, which is deterministic: the parent
// delegates, the child asks for its command, the parent's turn closes, the parent answers, the child
// resumes and runs, and the parent closes.
func TestADelegatedChildAsksItsParentBeforeItRuns(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "marker")
	approver := &countingApprover{}

	delegate := call(t, "c1", "delegate", map[string]any{
		"target": stack.AgentName, "input": "write the marker", "mode": "sync",
	})
	bash := call(t, "c2", "bash", map[string]any{
		"command": "echo x >> " + marker, "workdir": workspace,
	})
	// The child's id is minted by the delegation (an edge number and a random suffix), so it is
	// read from the tool result the way a caller reads it — the reply that answers it is filled in
	// after the first turn.
	approve := call(t, "c3", "delegation_answer", map[string]any{
		"session": "pending", "decision": "approve",
	})

	s := stack.OpenConfig(t, stack.Config{
		Graph:  "graph-delegation.yaml",
		Agents: []host.AgentsOption{host.WithApproval(host.Approval{Approver: approver})},
	},
		stack.Reply{ToolCall: &delegate},
		stack.Reply{ToolCall: &bash},
		stack.Reply{Text: "the child asked; I will answer next turn"},
		stack.Reply{ToolCall: &approve},
		stack.Reply{Text: "the marker is written"},
		stack.Reply{Text: "finished"},
	)
	s.OpenSessionWith(t, stack.SessionID, stack.DelegatingAuthority())

	first := s.Run(t, stack.SessionID, "do the work")
	child := childSession(t, first)
	approve = call(t, "c3", "delegation_answer", map[string]any{
		"session": child, "decision": "approve",
	})
	s.Model.Set(3, stack.Reply{ToolCall: &approve})
	// The child stopped to ask before running anything, and the deployment's approver was not the
	// one asked.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran before the parent approved it")
	}
	if len(approver.asked) != 0 {
		t.Fatalf("the deployment's approver was asked %d times; the parent should decide", len(approver.asked))
	}

	s.Run(t, stack.SessionID, "yes, go ahead")
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the approved command never ran: %v", err)
	}
	// Exactly once: a suspended step is re-entered, and a replay would append a second line.
	if strings.Count(string(body), "x") != 1 {
		t.Fatalf("marker = %q, want one line — the command ran more than once", body)
	}
	if len(approver.asked) != 0 {
		t.Fatalf("the deployment's approver was asked %d times after the answer", len(approver.asked))
	}
}

// childSession reads the address the delegate call returned, the way a caller does.
func childSession(t *testing.T, events []agent.Event) string {
	t.Helper()
	for _, event := range events {
		result, ok := event.(agent.ToolResult)
		if !ok {
			continue
		}
		var payload struct {
			Session  string `json:"session"`
			Status   string `json:"status"`
			Question string `json:"question"`
		}
		if err := json.Unmarshal([]byte(result.Result.Content.Text()), &payload); err != nil {
			continue
		}
		if payload.Session == "" || payload.Status != "input-required" {
			continue
		}
		if !strings.Contains(payload.Question, "approval") {
			t.Fatalf("question = %q, want the approval question", payload.Question)
		}
		return payload.Session
	}
	t.Fatal("the delegate call returned no paused child")
	return ""
}

// call builds one tool call the scripted model will ask for.
func call(t *testing.T, id, name string, args map[string]any) message.ToolCall {
	t.Helper()
	built, err := message.NewToolCall(id, name, args)
	if err != nil {
		t.Fatalf("tool call %s: %v", name, err)
	}
	return built
}
