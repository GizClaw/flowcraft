package hook

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	yamlv3 "gopkg.in/yaml.v3"
)

type contextProvider struct {
	request sdkmemory.ContextRequest
	result  sdkmemory.ContextResult
}

func (p *contextProvider) Context(_ context.Context, request sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	p.request = request
	if p.result.Items != nil {
		return p.result, nil
	}
	return sdkmemory.ContextResult{Items: []sdkmemory.ContextItem{{
		ID: "fact-1", Kind: sdkmemory.ContextFact, Score: 0.8,
		Content:     message.Content{Parts: []message.Part{message.TextPart{Text: "remembered"}}},
		Sources:     []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "conversation/message"}},
		SourceClass: sdkmemory.ContextSourceLongTerm,
	}}}, nil
}

type turnSink struct {
	turns []sdkmemory.Turn
}

func (s *turnSink) CommitTurn(_ context.Context, turn sdkmemory.Turn) error {
	s.turns = append(s.turns, turn)
	return nil
}

type fakeAssembly struct {
	provider *contextProvider
	sink     *turnSink
}

func (a *fakeAssembly) Context(ctx context.Context, request sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	return a.provider.Context(ctx, request)
}

func (a *fakeAssembly) CommitTurn(ctx context.Context, turn sdkmemory.Turn) error {
	return a.sink.CommitTurn(ctx, turn)
}

func (*fakeAssembly) PutDocument(context.Context, sdkmemory.Document) error { return nil }

func newFakeAssembly(t *testing.T) (sdkmemory.Assembly, *contextProvider, *turnSink) {
	t.Helper()
	provider := &contextProvider{}
	sink := &turnSink{}
	return &fakeAssembly{provider: provider, sink: sink}, provider, sink
}

func TestContextPreparerClonesBoardAndUsesRequest(t *testing.T) {
	assembly, provider, _ := newFakeAssembly(t)
	preparer, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  current_message: true
scope:
  runtime_id: memory
  user_id: tenant
dataset_ids: [docs]
budget:
  max_items: 4
  max_tokens: 100
min_score: 0.2
output: recalled
`),
		Deps: map[string]any{depName: assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := agent.NewBoard()
	req := &agent.Request{
		ContextID: "conversation",
		Message:   message.NewTextMessage(message.RoleUser, "find alpha"),
	}
	next, err := preparer.Before(context.Background(), agent.Identity{RunID: "run"}, req, previous)
	if err != nil {
		t.Fatal(err)
	}
	if next == previous {
		t.Fatal("preparer returned the input board")
	}
	if _, ok := previous.GetVar("recalled"); ok {
		t.Fatal("preparer mutated the input board")
	}
	items, ok := agent.GetTyped[[]sdkmemory.ContextItem](next, "recalled")
	if !ok || len(items) != 1 {
		t.Fatalf("recalled = %#v", items)
	}
	if provider.request.Query != "find alpha" ||
		provider.request.ConversationID != "conversation" ||
		provider.request.Scope.UserID != "tenant" {
		t.Fatalf("context request = %+v", provider.request)
	}
}

func TestContextPreparerRendersDefaultGoTemplateToContent(t *testing.T) {
	assembly, _, _ := newFakeAssembly(t)
	preparer, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  literal: alpha
scope:
  runtime_id: memory
output: recalled
render:
  output: memory_content
  gotmpl: {}
`),
		Deps: map[string]any{depName: assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := agent.NewBoard()
	next, err := preparer.Before(context.Background(), agent.Identity{RunID: "run"}, &agent.Request{}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := previous.GetVar("memory_content"); exists {
		t.Fatal("preparer mutated previous board")
	}
	items, ok := agent.GetTyped[[]sdkmemory.ContextItem](next, "recalled")
	if !ok || len(items) != 1 {
		t.Fatalf("typed output = %#v", items)
	}
	content, ok := agent.GetTyped[message.Content](next, "memory_content")
	if !ok {
		t.Fatalf("rendered output has unexpected type %T", boardValue(next, "memory_content"))
	}
	if text := content.Text(); !strings.Contains(text, "<memory_context>") || !strings.Contains(text, "remembered") {
		t.Fatalf("rendered output = %q", text)
	}
}

func TestContextPreparerRendersCustomGoTemplate(t *testing.T) {
	assembly, _, _ := newFakeAssembly(t)
	preparer, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  literal: alpha
scope:
  runtime_id: memory
output: recalled
render:
  output: memory_content
  gotmpl:
    template: '{{ range .Items }}{{ contentText .Content }}:{{ score .Score }}{{ end }}'
`),
		Deps: map[string]any{depName: assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	next, err := preparer.Before(context.Background(), agent.Identity{}, &agent.Request{}, agent.NewBoard())
	if err != nil {
		t.Fatal(err)
	}
	content, ok := agent.GetTyped[message.Content](next, "memory_content")
	if !ok || content.Text() != "remembered:0.800" {
		t.Fatalf("rendered output = %#v", content)
	}
}

func TestCloneItemsOwnsMutableData(t *testing.T) {
	original := []sdkmemory.ContextItem{{
		ID:       "summary",
		Kind:     sdkmemory.ContextSummary,
		Content:  message.Content{Parts: []message.Part{message.TextPart{Text: "summary"}}},
		Sources:  []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message"}},
		Metadata: sdkmemory.Metadata{"key": "value"},
		Hint: &sdkmemory.ExpandHint{
			Topics:     []string{"architecture"},
			SourceRefs: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "hint-message"}},
		},
	}}

	cloned := cloneItems(original)
	cloned[0].Content.Parts[0] = message.TextPart{Text: "changed"}
	cloned[0].Sources[0].ID = "changed"
	cloned[0].Metadata["key"] = "changed"
	cloned[0].Hint.Topics[0] = "changed"
	cloned[0].Hint.SourceRefs[0].ID = "changed"

	if got := original[0].Content.Parts[0].(message.TextPart).Text; got != "summary" {
		t.Fatalf("content = %q, want summary", got)
	}
	if original[0].Sources[0].ID != "message" || original[0].Metadata["key"] != "value" {
		t.Fatal("clone aliases source or metadata")
	}
	if original[0].Hint.Topics[0] != "architecture" || original[0].Hint.SourceRefs[0].ID != "hint-message" {
		t.Fatal("clone aliases expand hint")
	}
}

func TestContextPreparerSupportsRecentOnly(t *testing.T) {
	assembly, provider, _ := newFakeAssembly(t)
	preparer, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  recent_only: true
scope:
  runtime_id: memory
conversation_id: conversation
budget:
  max_chars: 50
output: recalled
`),
		Deps: map[string]any{depName: assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = preparer.Before(context.Background(), agent.Identity{}, &agent.Request{}, agent.NewBoard())
	if err != nil {
		t.Fatal(err)
	}
	if provider.request.Query != "" || provider.request.ConversationID != "conversation" ||
		provider.request.Budget.MaxChars != 50 {
		t.Fatalf("recent-only request = %+v", provider.request)
	}
}

func TestTurnCommitterUsesRunIDAndChannel(t *testing.T) {
	assembly, _, sink := newFakeAssembly(t)
	committer, err := TurnCommitterFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
scope:
  runtime_id: memory
  user_id: tenant
`),
		Deps: map[string]any{depName: assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "hello"))
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "world"))
	req := &agent.Request{ContextID: "conversation"}
	result := &agent.Result{LastBoard: board}
	id := agent.Identity{RunID: "run-42"}
	if err := committer.Commit(context.Background(), id, req, result); err != nil {
		t.Fatal(err)
	}
	if err := committer.Commit(context.Background(), id, req, result); err != nil {
		t.Fatal(err)
	}
	if len(sink.turns) != 2 {
		t.Fatalf("commits = %d, want 2", len(sink.turns))
	}
	turn := sink.turns[0]
	if turn.ConversationID != "conversation" ||
		turn.IdempotencyKey != "run-42" ||
		turn.Scope.UserID != "tenant" {
		t.Fatalf("turn = %+v", turn)
	}
	if len(turn.Messages) != 2 ||
		turn.Messages[0].Content.Text() != "hello" ||
		turn.Messages[1].Content.Text() != "world" {
		t.Fatalf("turn messages = %+v", turn.Messages)
	}
}

func TestHookValidationRejectsAmbiguousQuery(t *testing.T) {
	assembly, _, _ := newFakeAssembly(t)
	_, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  literal: alpha
  current_message: true
scope:
  runtime_id: memory
output: recalled
`),
		Deps: map[string]any{depName: assembly},
	})
	if err == nil {
		t.Fatal("accepted ambiguous query")
	}
}

func TestContextPreparerRejectsInvalidRenderSettings(t *testing.T) {
	assembly, _, _ := newFakeAssembly(t)
	for name, source := range map[string]string{
		"missing renderer": `
query: {literal: alpha}
scope: {runtime_id: memory}
output: recalled
render: {output: memory_content}
`,
		"same output": `
query: {literal: alpha}
scope: {runtime_id: memory}
output: recalled
render: {output: recalled, gotmpl: {}}
`,
		"invalid template": `
query: {literal: alpha}
scope: {runtime_id: memory}
output: recalled
render:
  output: memory_content
  gotmpl: {template: '{{'}
`,
		"unknown renderer field": `
query: {literal: alpha}
scope: {runtime_id: memory}
output: recalled
render: {output: memory_content, arbitrary: {}}
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
				Settings: settingsNode(t, source), Deps: map[string]any{depName: assembly},
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func settingsNode(t *testing.T, source string) *yamlv3.Node {
	t.Helper()
	var document yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(source), &document); err != nil {
		t.Fatal(err)
	}
	return document.Content[0]
}

func boardValue(board *agent.Board, key string) any {
	value, _ := board.GetVar(key)
	return value
}
