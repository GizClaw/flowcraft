package hook

import (
	"context"
	"strings"
	"testing"

	rootmemory "github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/sources"
	docsource "github.com/GizClaw/flowcraft/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	memoryconfig "github.com/GizClaw/flowcraft/sdkx/memory/config"
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

func TestContextPreparerClonesBoardAndUsesRequest(t *testing.T) {
	system, _, provider := testSystem(t)
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
		Deps: map[string]any{depName: &memoryconfig.Assembly{System: system}},
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
	system, _, _ := testSystem(t)
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
		Deps: map[string]any{depName: system},
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
	system, _, _ := testSystem(t)
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
		Deps: map[string]any{depName: system},
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
	system, _, provider := testSystem(t)
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
		Deps: map[string]any{depName: system},
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

func TestContextPreparerRejectsInvalidProviderKinds(t *testing.T) {
	system, _, provider := testSystem(t)
	provider.result = sdkmemory.ContextResult{Items: []sdkmemory.ContextItem{{
		ID: "fact-1", Kind: sdkmemory.ContextItemKind("unknown"), Score: 0.8,
		Content:     message.Content{Parts: []message.Part{message.TextPart{Text: "remembered"}}},
		Sources:     []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "conversation/message"}},
		SourceClass: sdkmemory.ContextSourceLongTerm,
	}}}
	preparer, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  literal: alpha
scope:
  runtime_id: memory
output: recalled
`),
		Deps: map[string]any{depName: system},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = preparer.Before(
		context.Background(),
		agent.Identity{RunID: "run"},
		&agent.Request{},
		agent.NewBoard(),
	)
	if err == nil {
		t.Fatal("ContextPreparer accepted an unknown context item kind")
	}
	if !sdkmemory.IsKind(err, sdkmemory.KindInternal) {
		t.Fatalf("ContextPreparer error = %v, want provider contract failure", err)
	}
}

func TestTurnCommitterUsesRunIDAndIsIdempotent(t *testing.T) {
	system, messages, _ := testSystem(t)
	committer, err := TurnCommitterFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
scope:
  runtime_id: memory
  user_id: tenant
`),
		Deps: map[string]any{depName: system},
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
	commits, err := messages.ListCommits(
		context.Background(),
		sdkmemory.Scope{RuntimeID: "memory", UserID: "tenant"},
		"conversation",
		msgsource.ListCommitOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || len(commits[0].Records) != 2 {
		t.Fatalf("commits = %+v", commits)
	}
}

func TestHookValidationRejectsAmbiguousQuery(t *testing.T) {
	system, _, _ := testSystem(t)
	_, err := ContextPreparerFactory(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, `
query:
  literal: alpha
  current_message: true
scope:
  runtime_id: memory
output: recalled
`),
		Deps: map[string]any{depName: system},
	})
	if err == nil {
		t.Fatal("accepted ambiguous query")
	}
}

func TestContextPreparerRejectsInvalidRenderSettings(t *testing.T) {
	system, _, _ := testSystem(t)
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
				Settings: settingsNode(t, source), Deps: map[string]any{depName: system},
			})
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func testSystem(t *testing.T) (*rootmemory.System, *msgsource.WorkspaceStore, *contextProvider) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	messages, err := msgsource.NewWorkspaceStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := docsource.NewWorkspaceStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	provider := &contextProvider{}
	catalog, err := sources.NewWorkspaceScopeCatalog(ws)
	if err != nil {
		t.Fatal(err)
	}
	system, err := rootmemory.NewSystem(messages, documents, catalog, provider)
	if err != nil {
		t.Fatal(err)
	}
	return system, messages, provider
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
