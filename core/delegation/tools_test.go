package delegation_test

import (
	"context"
	"iter"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/delegation"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

func findTool(t *testing.T, service *delegation.Service, name string) tool.Tool {
	t.Helper()
	for _, candidate := range delegation.Tools(service).Tools() {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("tool %q is not exposed", name)
	return nil
}

func TestDelegateToolReturnsTheChildAnswer(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	content, err := findTool(t, service, "delegate").Execute(
		parentCtx("call-1"), `{"target":"worker","input":"do it"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if content.Text() != "done" {
		t.Fatalf("content = %q", content.Text())
	}
}

func TestTargetsToolListsTheAllowlist(t *testing.T) {
	sessions, _ := newFake()
	service, _ := delegation.New(sessions, delegation.Policy{Targets: []string{"worker"}})
	content, err := findTool(t, service, "delegation_targets").Execute(
		parentCtx("call-1"), `{}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var raw []byte
	for _, part := range content.Parts {
		if data, ok := part.(message.DataPart); ok {
			raw = data.Value
			break
		}
	}
	if len(raw) == 0 {
		t.Fatalf("no data part in %+v", content.Parts)
	}
	var body struct {
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Targets) != 1 || body.Targets[0] != "worker" {
		t.Fatalf("targets = %v", body.Targets)
	}
}

// The tool source is a plain tool.Source: the host can inject it without
// knowing the delegation package.
var _ tool.Source = delegation.Tools(nil)
var _ = context.Background

func runTool(t *testing.T, candidate tool.Tool, ctx context.Context, args string) message.Content {
	t.Helper()
	content, err := candidate.Execute(ctx, args)
	if err != nil {
		t.Fatalf("%s: %v", candidate.Definition().Name, err)
	}
	return content
}

func dataObject(t *testing.T, content message.Content) map[string]any {
	t.Helper()
	for _, part := range content.Parts {
		if data, ok := part.(message.DataPart); ok {
			var body map[string]any
			if err := json.Unmarshal(data.Value, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return body
		}
	}
	t.Fatalf("no data part in %+v", content.Parts)
	return nil
}

func TestDelegationStatusTool(t *testing.T) {
	sessions, child := newFake()
	started := make(chan struct{})
	release := make(chan struct{})
	child.run = func(f *fakeSession, _ context.Context) iter.Seq2[agent.Event, error] {
		return func(yield func(agent.Event, error) bool) {
			close(started)
			<-release
			f.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
	service, _ := delegation.New(sessions, asyncPolicy())
	defer func() { _ = service.Close() }()
	ctx := parentCtx("call-1")

	submitted := dataObject(t, runTool(t, findTool(t, service, "delegate"), ctx,
		`{"mode":"async","target":"worker","input":"do it"}`))
	id, _ := submitted["delegation_id"].(string)
	if id == "" {
		t.Fatalf("delegation_id = %v", submitted)
	}
	<-started

	status := dataObject(t, runTool(t, findTool(t, service, "delegation_status"), ctx,
		`{"delegation_id":"`+id+`"}`))
	if state := status["status"]; state != "running" && state != "accepted" {
		t.Fatalf("status = %v", state)
	}

	close(release)
	waitStatus(t, service, ctx, id, delegation.StatusCompleted)
	if got := runTool(t, findTool(t, service, "delegation_status"), ctx,
		`{"delegation_id":"`+id+`"}`).Text(); got != "done" {
		t.Fatalf("completed status = %q", got)
	}
}

func TestDelegationAnswerTool(t *testing.T) {
	sessions, child := newFake()
	question := message.NewTextContent("which?")
	done := agent.Result{State: agent.StateCompleted, Content: message.NewTextContent("done")}
	child.result = agent.Result{State: agent.StateInputRequired, Question: &question}
	child.run = func(f *fakeSession, _ context.Context) iter.Seq2[agent.Event, error] {
		return func(yield func(agent.Event, error) bool) {
			f.mu.Lock()
			run := f.runs
			f.mu.Unlock()
			if run == 1 {
				f.setState(agent.StateInputRequired)
				yield(agent.Status{State: agent.StateInputRequired}, nil)
				return
			}
			f.mu.Lock()
			f.result = done
			f.mu.Unlock()
			f.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
	service, _ := delegation.New(sessions, asyncPolicy())
	defer func() { _ = service.Close() }()
	ctx := parentCtx("call-1")

	submitted := dataObject(t, runTool(t, findTool(t, service, "delegate"), ctx,
		`{"mode":"async","target":"worker","input":"do it"}`))
	id, _ := submitted["delegation_id"].(string)
	waitStatus(t, service, ctx, id, delegation.StatusInputRequired)

	paused := dataObject(t, runTool(t, findTool(t, service, "delegation_status"), ctx,
		`{"delegation_id":"`+id+`"}`))
	if paused["question"] != "which?" {
		t.Fatalf("status = %v", paused)
	}

	resumed := dataObject(t, runTool(t, findTool(t, service, "delegation_answer"), ctx,
		`{"delegation_id":"`+id+`","answer":"yes"}`))
	if resumed["status"] != "running" {
		t.Fatalf("answer status = %v", resumed)
	}
	waitStatus(t, service, ctx, id, delegation.StatusCompleted)
	if got := runTool(t, findTool(t, service, "delegation_status"), ctx,
		`{"delegation_id":"`+id+`"}`).Text(); got != "done" {
		t.Fatalf("completed status = %q", got)
	}
}

func TestDelegationCancelTool(t *testing.T) {
	sessions, child := newFake()
	started := make(chan struct{})
	child.stop = make(chan struct{})
	child.run = func(f *fakeSession, ctx context.Context) iter.Seq2[agent.Event, error] {
		return func(yield func(agent.Event, error) bool) {
			close(started)
			select {
			case <-ctx.Done():
			case <-f.stop:
			}
			f.setState(agent.StateCanceled)
			yield(agent.Status{State: agent.StateCanceled}, nil)
		}
	}
	service, _ := delegation.New(sessions, asyncPolicy())
	defer func() { _ = service.Close() }()
	ctx := parentCtx("call-1")
	submitted := dataObject(t, runTool(t, findTool(t, service, "delegate"), ctx,
		`{"mode":"async","target":"worker","input":"do it"}`))
	id, _ := submitted["delegation_id"].(string)
	<-started

	canceled := dataObject(t, runTool(t, findTool(t, service, "delegation_cancel"), ctx,
		`{"delegation_id":"`+id+`"}`))
	if canceled["status"] != "canceled" {
		t.Fatalf("cancel status = %v", canceled)
	}
	status := dataObject(t, runTool(t, findTool(t, service, "delegation_status"), ctx,
		`{"delegation_id":"`+id+`"}`))
	if status["status"] != "canceled" {
		t.Fatalf("status = %v", status)
	}
}
