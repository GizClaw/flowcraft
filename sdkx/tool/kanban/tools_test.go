package kanban_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkkanban "github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/tool"
	kanbantool "github.com/GizClaw/flowcraft/sdkx/tool/kanban"
)

func newKanban(t *testing.T) *sdkkanban.Kanban {
	t.Helper()
	k := sdkkanban.New("scope-test", sdkkanban.WithMaxPending(100))
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func decode(t *testing.T, out string) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("result is not a JSON object: %v\n%s", err, out)
	}
	return m
}

func schemaProperties(t *testing.T, def tool.Definition) map[string]json.RawMessage {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("input schema is not a JSON object: %v\n%s", err, def.InputSchema)
	}
	return schema.Properties
}

func TestSubmitToolDefinition(t *testing.T) {
	def := (&kanbantool.SubmitTool{}).Definition()
	if def.Name != kanbantool.SubmitName {
		t.Fatalf("Name = %q, want %q", def.Name, kanbantool.SubmitName)
	}

	// The board only queues work; nothing here executes it. A
	// description promising a callback would strand the model waiting
	// for a message that arrives only if a host happens to be wired.
	lowered := strings.ToLower(def.Description)
	for _, forbidden := range []string{"callback", "in the background"} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("description promises %q, which the board does not guarantee: %s",
				forbidden, def.Description)
		}
	}

	// Scheduling moved to sdkx/kanban/scheduler; the tool must not
	// advertise parameters it no longer forwards.
	props := schemaProperties(t, def)
	for _, gone := range []string{"delay", "cron", "timezone"} {
		if _, ok := props[gone]; ok {
			t.Errorf("schema still declares %q", gone)
		}
	}
}

func TestSubmitToolQueuesCard(t *testing.T) {
	k := newKanban(t)
	x := &kanbantool.SubmitTool{Kanban: k}

	out, err := x.Execute(context.Background(), `{
		"target_agent_id": "builder",
		"query": "create the app",
		"user_query": "make me an app",
		"dispatch_note": "report back when done"
	}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := decode(t, out)
	if got["status"] != string(sdkkanban.StatusPending) {
		t.Errorf("status = %q, want pending", got["status"])
	}
	if got["target_agent_id"] != "builder" {
		t.Errorf("target_agent_id = %q", got["target_agent_id"])
	}

	card, ok := k.Card(got["card_id"])
	if !ok {
		t.Fatalf("card_id %q is not on the board", got["card_id"])
	}
	if card.Task.Query != "create the app" {
		t.Errorf("Query = %q", card.Task.Query)
	}
	if card.Task.UserQuery != "make me an app" {
		t.Errorf("UserQuery = %q", card.Task.UserQuery)
	}
	if card.Task.DispatchNote != "report back when done" {
		t.Errorf("DispatchNote = %q", card.Task.DispatchNote)
	}
}

func TestSubmitToolReportsMissingBoard(t *testing.T) {
	_, err := (&kanbantool.SubmitTool{}).Execute(
		context.Background(), `{"target_agent_id":"a","query":"q"}`,
	)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want not available", err)
	}
}

func TestSubmitToolPropagatesValidationFailure(t *testing.T) {
	k := newKanban(t)
	x := &kanbantool.SubmitTool{Kanban: k}

	// The board requires a target; the tool must surface that rather
	// than reporting a queued card.
	if _, err := x.Execute(context.Background(), `{"query":"q"}`); !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestSubmitToolRejectsMalformedArguments(t *testing.T) {
	k := newKanban(t)
	x := &kanbantool.SubmitTool{Kanban: k}
	if _, err := x.Execute(context.Background(), `not json`); err == nil {
		t.Fatal("Execute accepted malformed arguments")
	}
}

func TestSubmitToolRecordsProducer(t *testing.T) {
	k := newKanban(t)
	x := &kanbantool.SubmitTool{Kanban: k}
	ctx := sdkkanban.WithProducerID(context.Background(), "planner")

	out, err := x.Execute(ctx, `{"target_agent_id":"builder","query":"q"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	card, _ := k.Card(decode(t, out)["card_id"])
	if card.Producer != "planner" {
		t.Errorf("Producer = %q, want planner", card.Producer)
	}
}

func TestSubmitToolDeclaresStateMutation(t *testing.T) {
	if !(&kanbantool.SubmitTool{}).Metadata().MutatesState {
		t.Error("MutatesState = false; submitting places a card others can see")
	}
}

func TestBoardResolvedFromContext(t *testing.T) {
	k := newKanban(t)
	ctx := kanbantool.WithKanban(context.Background(), k)

	if got := kanbantool.KanbanFrom(ctx); got != k {
		t.Fatalf("KanbanFrom = %v, want %v", got, k)
	}
	out, err := (&kanbantool.SubmitTool{}).Execute(ctx,
		`{"target_agent_id":"a","query":"q"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, ok := k.Card(decode(t, out)["card_id"]); !ok {
		t.Error("card not found on the context-resolved board")
	}
}

func TestKanbanFromAbsent(t *testing.T) {
	if got := kanbantool.KanbanFrom(context.Background()); got != nil {
		t.Errorf("KanbanFrom = %v, want nil", got)
	}
}

// A struct field wins over the context so a caller holding a specific
// board is never surprised by an ambient one.
func TestStructFieldWinsOverContext(t *testing.T) {
	field := newKanban(t)
	ambient := newKanban(t)
	ctx := kanbantool.WithKanban(context.Background(), ambient)

	x := &kanbantool.SubmitTool{Kanban: field}
	out, err := x.Execute(ctx, `{"target_agent_id":"a","query":"q"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	id := decode(t, out)["card_id"]
	if _, ok := field.Card(id); !ok {
		t.Error("card did not land on the field-supplied board")
	}
	if _, ok := ambient.Card(id); ok {
		t.Error("card landed on the context board despite an explicit field")
	}
}

func TestTaskContextToolRendersCard(t *testing.T) {
	k := newKanban(t)
	card, err := k.Submit(context.Background(), sdkkanban.Task{
		TargetAgentID: "researcher",
		Query:         "find the figures",
		UserQuery:     "how did last quarter go",
		DispatchNote:  "I lack data access",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	k.Claim(card.ID, "worker-1")
	k.Done(card.ID, sdkkanban.Result{Output: "revenue up 4%"})

	out, err := (&kanbantool.TaskContextTool{Kanban: k}).Execute(
		context.Background(), `{"card_id":"`+card.ID+`"}`,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{
		card.ID, "find the figures", "how did last quarter go",
		"I lack data access", "revenue up 4%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendering missing %q:\n%s", want, out)
		}
	}
}

func TestTaskContextToolUnknownCard(t *testing.T) {
	k := newKanban(t)
	_, err := (&kanbantool.TaskContextTool{Kanban: k}).Execute(
		context.Background(), `{"card_id":"missing"}`,
	)
	if !errdefs.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestTaskContextToolReportsMissingBoard(t *testing.T) {
	_, err := (&kanbantool.TaskContextTool{}).Execute(
		context.Background(), `{"card_id":"x"}`,
	)
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want not available", err)
	}
}

func TestTaskContextToolDefinition(t *testing.T) {
	def := (&kanbantool.TaskContextTool{}).Definition()
	if def.Name != kanbantool.TaskContextName {
		t.Fatalf("Name = %q, want %q", def.Name, kanbantool.TaskContextName)
	}
	if _, ok := schemaProperties(t, def)["card_id"]; !ok {
		t.Error("schema does not declare card_id")
	}
}
