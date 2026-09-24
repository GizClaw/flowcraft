package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/lines/chat"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/verify"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/memory/hook"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/workspace"
)

const testSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "recent": {"max_items": 8, "max_tokens": 4096},
  "interval": "0"
}`

var testNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func newTestAssembly(t *testing.T, settings string) (*Assembly, workspace.Workspace) {
	t.Helper()
	if settings == "" {
		settings = testSettings
	}
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return newTestAssemblyOn(t, ws, settings), ws
}

func newTestAssemblyWith(t *testing.T, settings string, options ...Option) (*Assembly, workspace.Workspace) {
	t.Helper()
	if settings == "" {
		settings = testSettings
	}
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return newTestAssemblyOn(t, ws, settings, options...), ws
}

func newTestAssemblyOn(t *testing.T, ws workspace.Workspace, settings string, options ...Option) *Assembly {
	t.Helper()
	if settings == "" {
		settings = testSettings
	}
	factoryOptions := append([]Option{WithClock(func() time.Time { return testNow })}, options...)
	value, err := NewFactory(factoryOptions...).New(
		context.Background(),
		resource.Input{Settings: []byte(settings), Deps: map[string]any{"workspace": ws}},
	)
	if err != nil {
		t.Fatal(err)
	}
	assembly, ok := value.(*Assembly)
	if !ok {
		t.Fatalf("factory returned %T, want *Assembly", value)
	}
	if err := assembly.Wire(context.Background()); err != nil {
		t.Fatal(err)
	}
	return assembly
}

func testScope() corememory.Scope {
	return corememory.Scope{RuntimeID: "memories", UserID: "u1"}
}

// TestAssemblyCloseBeforeWireIsFinal pins the lifecycle gap: closing an
// unwired assembly used to be a silent no-op after which Wire could still
// start a runner over pools Close had already released. Close is now final
// (and idempotent), and Wire refuses to run afterwards.
func TestAssemblyCloseBeforeWireIsFinal(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	value, err := NewFactory(WithClock(func() time.Time { return testNow })).New(
		context.Background(),
		resource.Input{Settings: []byte(testSettings), Deps: map[string]any{"workspace": ws}},
	)
	if err != nil {
		t.Fatal(err)
	}
	assembly, ok := value.(*Assembly)
	if !ok {
		t.Fatalf("factory returned %T, want *Assembly", value)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := assembly.Wire(context.Background()); err == nil {
		t.Fatal("Wire after Close succeeded, want a closed-assembly error")
	}
}

func textMessage(role coremessage.Role, text string) coremessage.Message {
	return coremessage.Message{
		Role:    role,
		Content: coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
	}
}

func TestFactorySpecAndRegister(t *testing.T) {
	factory := NewFactory()
	spec := factory.Spec()
	if spec.Kind != corememory.AssemblyKind {
		t.Fatalf("kind = %q, want %q", spec.Kind, corememory.AssemblyKind)
	}
	if spec.Impl != Impl {
		t.Fatalf("impl = %q, want %q", spec.Impl, Impl)
	}
	registry := resource.NewRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(corememory.AssemblyKind, Impl); !ok {
		t.Fatal("factory was not registered")
	}
}

func TestHookFactoriesBindAssembly(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	preparer, err := hook.ContextPreparer{}.New(ctx, resource.Input{
		Settings: []byte(`{
			"query": {"literal": "hello"},
			"scope": {"runtime_id": "memories", "user_id": "u1"},
			"conversation_id": "conv-1",
			"output": "memory_items"
		}`),
		Deps: map[string]any{"memory": assembly},
	})
	if err != nil {
		t.Fatalf("context hook: %v", err)
	}
	if _, ok := preparer.(agent.PreparerFunc); !ok {
		t.Fatalf("context hook returned %T, want agent.PreparerFunc", preparer)
	}
	committer, err := hook.TurnCommitter{}.New(ctx, resource.Input{
		Settings: []byte(`{
			"scope": {"runtime_id": "memories", "user_id": "u1"},
			"conversation_id": "conv-1"
		}`),
		Deps: map[string]any{"memory": assembly},
	})
	if err != nil {
		t.Fatalf("turn hook: %v", err)
	}
	if _, ok := committer.(agent.CommitterFunc); !ok {
		t.Fatalf("turn hook returned %T, want agent.CommitterFunc", committer)
	}
}

// TestHookFactoriesDriveRealAssembly drives the memory.turn and memory.context
// hook factories through the real assembly (not a fake): commit, idempotent
// replay, then recall into the prepared board.
func TestHookFactoriesDriveRealAssembly(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	committerFactory, err := hook.TurnCommitter{}.New(ctx, resource.Input{
		Settings: []byte(`{
			"scope": {"runtime_id": "memories", "user_id": "u1"},
			"conversation_id": "conv-1"
		}`),
		Deps: map[string]any{"memory": assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	committer, ok := committerFactory.(agent.CommitterFunc)
	if !ok {
		t.Fatalf("committer = %T, want agent.CommitterFunc", committerFactory)
	}
	board := agent.NewBoard()
	board.SetChannel(agent.MainChannel, []coremessage.Message{textMessage(coremessage.RoleUser, "hello memory")})
	run := &agent.Result{RunID: "run-1", LastBoard: board}
	identity := agent.Identity{RunID: "run-1"}
	request := &agent.Request{ContextID: "conv-1"}
	if err := committer.Commit(ctx, identity, request, run); err != nil {
		t.Fatal(err)
	}
	records, err := assembly.MessageStore().Latest(ctx, testScope(), "conv-1", msgsource.LatestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 after hook commit", len(records))
	}
	if err := committer.Commit(ctx, identity, request, run); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	records, err = assembly.MessageStore().Latest(ctx, testScope(), "conv-1", msgsource.LatestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 after replay", len(records))
	}
	preparerFactory, err := hook.ContextPreparer{}.New(ctx, resource.Input{
		Settings: []byte(`{
			"query": {"literal": "hello"},
			"scope": {"runtime_id": "memories", "user_id": "u1"},
			"conversation_id": "conv-1",
			"output": "memory_items"
		}`),
		Deps: map[string]any{"memory": assembly},
	})
	if err != nil {
		t.Fatal(err)
	}
	preparer, ok := preparerFactory.(agent.PreparerFunc)
	if !ok {
		t.Fatalf("preparer = %T, want agent.PreparerFunc", preparerFactory)
	}
	next, err := preparer.Before(ctx, agent.Identity{RunID: "run-2"}, &agent.Request{ContextID: "conv-1"}, agent.NewBoard())
	if err != nil {
		t.Fatal(err)
	}
	value, ok := next.GetVar("memory_items")
	if !ok {
		t.Fatal("memory_items was not written to the board")
	}
	items, ok := value.([]corememory.ContextItem)
	if !ok || len(items) == 0 {
		t.Fatalf("memory_items = %#v, want non-empty []corememory.ContextItem", value)
	}
	found := false
	for _, item := range items {
		if strings.Contains(item.Content.Text(), "hello memory") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("recalled items do not contain the committed turn: %d items", len(items))
	}
}

func TestCommitTurnAndRecentContext(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{
			textMessage(coremessage.RoleUser, "hello"),
			textMessage(coremessage.RoleAssistant, "hi there"),
		},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	records, err := assembly.MessageStore().Latest(ctx, testScope(), "conv-1", msgsource.LatestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Truncated {
		t.Fatalf("items = %d truncated = %v, want 2/false", len(result.Items), result.Truncated)
	}
	if result.Items[0].MessageRole != coremessage.RoleUser || result.Items[1].MessageRole != coremessage.RoleAssistant {
		t.Fatalf("roles = %q/%q", result.Items[0].MessageRole, result.Items[1].MessageRole)
	}
	if result.Items[0].Sequence != 1 || result.Items[1].Sequence != 2 {
		t.Fatalf("sequences = %d/%d", result.Items[0].Sequence, result.Items[1].Sequence)
	}
	if result.Items[0].SourceClass != corememory.ContextSourceRecent {
		t.Fatalf("source class = %q", result.Items[0].SourceClass)
	}
	if !result.Items[0].Timestamp.Equal(testNow) {
		t.Fatalf("timestamp = %v, want %v", result.Items[0].Timestamp, testNow)
	}
	// pack.RuneCounter estimates ceil(runes/4): "hello" and "hi there"
	// count as two tokens each.
	if result.TokenCount != 4 {
		t.Fatalf("token count = %d, want 4", result.TokenCount)
	}
}

// TestContextRecentBoundsOversizedNewestTurn pins the fallback recent lane:
// the newest turn survives a tight budget as bounded, truncated content
// instead of being injected unbounded.
func TestContextRecentBoundsOversizedNewestTurn(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	huge := strings.Repeat("x", 4000) // ~1000 estimated tokens
	if err := assembly.CommitTurn(ctx, corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-huge",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, huge)},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.contextRecent(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1",
		Budget: corememory.Budget{MaxItems: 2, MaxTokens: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items = %d, want the newest turn to survive", len(result.Items))
	}
	item := result.Items[0]
	if item.TokenCount > 64 || !result.Truncated {
		t.Fatalf("tokens/truncated = %d/%v, want <=64/true", item.TokenCount, result.Truncated)
	}
	if text := item.Content.Text(); text == "" || text == huge {
		t.Fatalf("content = %d runes, want a non-empty bounded prefix", len([]rune(text)))
	}
}

func TestCommitTurnIdempotentReplay(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "hello")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	records, err := assembly.MessageStore().Latest(ctx, testScope(), "conv-1", msgsource.LatestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 after replay", len(records))
	}
	// A retried run id is lenient by contract: the original commit wins and
	// the retry's different content is not appended.
	retry := turn.Clone()
	retry.Messages = []coremessage.Message{textMessage(coremessage.RoleUser, "different")}
	if err := assembly.CommitTurn(ctx, retry); err != nil {
		t.Fatalf("lenient replay failed: %v", err)
	}
	records, err = assembly.MessageStore().Latest(ctx, testScope(), "conv-1", msgsource.LatestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Message.Content.Text() != "hello" {
		t.Fatalf("records after lenient replay = %#v", records)
	}
}

func TestContextBudgetAndScopeIsolation(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{
			textMessage(coremessage.RoleUser, "one"),
			textMessage(coremessage.RoleAssistant, "two"),
			textMessage(coremessage.RoleUser, "three"),
			textMessage(coremessage.RoleAssistant, "four"),
		},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "one",
		Budget: corememory.Budget{MaxItems: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(result.Items))
	}
	if result.Items[0].Sequence != 3 || result.Items[1].Sequence != 4 {
		t.Fatalf("sequences = %d/%d, want the newest 3/4", result.Items[0].Sequence, result.Items[1].Sequence)
	}
	other := corememory.Scope{RuntimeID: "memories", UserID: "u1", AgentID: "other"}
	isolated, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: other, ConversationID: "conv-1", Query: "one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated.Items) != 0 {
		t.Fatalf("isolated items = %d, want 0", len(isolated.Items))
	}
}

func TestAssemblyDurabilityAcrossRebuild(t *testing.T) {
	assembly, ws := newTestAssembly(t, "")
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "persisted")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	rebuilt := newTestAssemblyOn(t, ws, "")
	result, err := rebuilt.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "persisted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Content.Text() != "persisted" {
		t.Fatalf("rebuilt items = %#v", result.Items)
	}
}

func TestPutDocumentPersistsLatestRevision(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	document := corememory.Document{
		Scope: testScope(), DatasetID: "docs", DocumentID: "doc-1", IdempotencyKey: "rev-1",
		Content:    coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "body"}}},
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceExternal, ID: "uri:test"}},
	}
	if err := assembly.PutDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	if err := assembly.PutDocument(ctx, document); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	revision, ok, err := assembly.DocumentStore().Get(ctx, testScope(), "docs", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || revision.Version != 1 || revision.Content.Text() != "body" {
		t.Fatalf("latest = %#v ok=%v", revision, ok)
	}
	document.IdempotencyKey = "rev-2"
	document.Content = coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "body v2"}}}
	if err := assembly.PutDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	revision, ok, err = assembly.DocumentStore().Get(ctx, testScope(), "docs", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || revision.Version != 2 || revision.Content.Text() != "body v2" {
		t.Fatalf("latest = %#v ok=%v", revision, ok)
	}
	scopes, err := assembly.Catalog().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].HardPartitionKey() != testScope().HardPartitionKey() {
		t.Fatalf("scopes = %#v", scopes)
	}
}

func TestFactoryRejectsInvalidSettings(t *testing.T) {
	tests := map[string]string{
		"unknown field":         `{"storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}}, "unknown": true}`,
		"bad log driver":        `{"storage": {"log": {"driver": "sqlite"}, "kv": {"driver": "workspace"}}}`,
		"negative recent":       `{"storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}}, "recent": {"max_items": -1}}`,
		"oversized recent":      `{"storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}}, "recent": {"max_tokens": 100000000}}`,
		"oversized chunk":       `{"storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}}, "chunk": {"max_runes": 100000000}}`,
		"oversized fact":        `{"storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}}, "fact": {"max_fact_chars": 100000000}}`,
		"missing scope runtime": `{"storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}}, "scopes": [{"user_id": "u1"}]}`,
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) {
			ws, err := workspace.NewLocalWorkspace(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ws.Close() })
			_, err = NewFactory().New(context.Background(), resource.Input{
				Settings: []byte(settings),
				Deps:     map[string]any{"workspace": ws},
			})
			if err == nil {
				t.Fatal("expected a settings error")
			}
		})
	}
}

// TestFactoryDefaultsStorageToWorkspace pins the documented default: a
// settings document without a storage block uses the workspace driver
// instead of failing validation.
func TestFactoryDefaultsStorageToWorkspace(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	value, err := NewFactory().New(context.Background(), resource.Input{
		Settings: []byte(`{"scopes":[{"runtime_id":"memories"}],"interval":"0"}`),
		Deps:     map[string]any{"workspace": ws},
	})
	if err != nil {
		t.Fatalf("default storage rejected: %v", err)
	}
	assembly, ok := value.(*Assembly)
	if !ok {
		t.Fatalf("factory returned %T", value)
	}
	if err := assembly.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPolicyDigestTracksDerivationNotOperations pins the watermark identity:
// retrieval-only knobs, lifecycle cadence, storage drivers, and seeded scopes
// must not change the digest, while derivation inputs and the projection
// namespace must.
func TestPolicyDigestTracksDerivationNotOperations(t *testing.T) {
	base := Settings{
		Generate:   ModelSettings{Provider: "deepseek", Name: "deepseek-v4-flash"},
		Fact:       FactSettings{Strategy: "simple", TailMaxChars: 15000, MaxFacts: 64, MaxFactChars: 2000, MaxQueryChars: 2000, MaxEmbeddingInputChars: 8000},
		Summary:    SummarySettings{ChunkSize: 10, CondenseThreshold: 6, GroupSize: 3, MaxDepth: 4},
		Chunk:      ChunkSettings{MaxRunes: 1600, OverlapRunes: 160},
		Projection: "facts",
	}
	baseDigest, err := policyDigest(base, "")
	if err != nil {
		t.Fatal(err)
	}
	operational := base
	operational.Storage = StorageSettings{
		Log: DriverSettings{Driver: DriverPostgres, Settings: json.RawMessage(`{"dsn":"postgres://example"}`)},
		KV:  DriverSettings{Driver: DriverPostgres, Settings: json.RawMessage(`{"dsn":"postgres://example"}`)},
	}
	operational.Recent = RecentSettings{MaxItems: 3, MaxTokens: 99}
	operational.Lanes = LanesSettings{BM25: LaneSettings{Weight: 0.9}}
	operational.Interval = "10s"
	operational.Scopes = []ScopeSettings{{RuntimeID: "other"}}
	operationalDigest, err := policyDigest(operational, "")
	if err != nil {
		t.Fatal(err)
	}
	if operationalDigest != baseDigest {
		t.Fatalf("operational settings changed the derivation digest")
	}
	derivation := base
	derivation.Fact.MaxFacts = 32
	if digest, err := policyDigest(derivation, ""); err != nil || digest == baseDigest {
		t.Fatalf("fact settings must change the digest (err=%v)", err)
	}
	derivation = base
	derivation.Projection = "facts-v2"
	if digest, err := policyDigest(derivation, ""); err != nil || digest == baseDigest {
		t.Fatalf("projection namespace must change the digest (err=%v)", err)
	}
}

// TestCustomDeriverChangesPolicyDigest pins the gap the review found: the
// watermarks that decide what has been derived are keyed by the policy digest,
// so a host that replaces the deriver without changing the digest keeps
// scopes looking up to date and they are never re-derived. A caller-supplied
// deriver therefore contributes a policy name -- its version when it names
// one, a conservative marker otherwise.
func TestCustomDeriverChangesPolicyDigest(t *testing.T) {
	build := func(t *testing.T, options ...Option) string {
		t.Helper()
		assembly, _ := newTestAssemblyWith(t, "", options...)
		return assembly.PolicyDigest()
	}
	base := build(t)
	if versioned := build(t, WithDeriver(fakeDeriver{}), WithDeriverVersion("fake-deriver-v1")); versioned == base {
		t.Fatal("a versioned custom deriver left the policy digest unchanged")
	} else if unnamed := build(t, WithDeriver(fakeDeriver{})); unnamed == base || unnamed == versioned {
		t.Fatalf("an unversioned custom deriver must still change the digest (base=%s versioned=%s unnamed=%s)",
			base[:8], versioned[:8], unnamed[:8])
	}
	if unnamed := build(t, WithDeriver(fakeDeriver{})); unnamed != build(t, WithDeriver(fakeDeriver{})) {
		t.Fatal("the same custom deriver must produce a stable digest")
	}
}

// fakeDeriver returns one deterministic fact for every commit so the
// derivation pipeline can be exercised without an inference provider.
type fakeDeriver struct {
	eventTime time.Time
	linkedIDs []string
}

// perInputDeriver derives one fact whose text depends on the commit content,
// so a multi-commit conversation produces distinct canonical facts.
type perInputDeriver struct{}

func (perInputDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	text := strings.TrimSpace(input.Content.Text())
	if text == "" {
		return nil, nil
	}
	hash := factview.CanonicalHash(text)
	metadata := corememory.Metadata{
		"canonical_hash":      hash,
		"entities":            `["Alice"]`,
		"event_time":          testNow.Format(time.RFC3339Nano),
		"source_digest":       factview.ComputeSourceDigest(input.Sources),
		"transform_signature": "per-input-derive-v1",
	}
	return []component.Artifact{{
		Kind:     chat.KindFact,
		ID:       "fact-" + strings.ReplaceAll(hash, ":", "-"),
		Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
		Sources:  append([]corememory.SourceRef(nil), input.Sources...),
		Metadata: metadata,
	}}, nil
}

func (deriver fakeDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	eventTime := deriver.eventTime
	if eventTime.IsZero() {
		eventTime = testNow
	}
	metadata := corememory.Metadata{
		"canonical_hash":      factview.CanonicalHash("Alice likes tea"),
		"entities":            `["Alice"]`,
		"event_time":          eventTime.Format(time.RFC3339Nano),
		"source_digest":       factview.ComputeSourceDigest(input.Sources),
		"transform_signature": "fake-derive-v1",
	}
	if len(deriver.linkedIDs) > 0 {
		encoded, _ := json.Marshal(deriver.linkedIDs)
		metadata["linked_memory_ids"] = string(encoded)
	}
	return []component.Artifact{{
		Kind:     chat.KindFact,
		ID:       "fact-alice-tea",
		Content:  coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: "Alice likes tea"}}},
		Sources:  append([]corememory.SourceRef(nil), input.Sources...),
		Metadata: metadata,
	}}, nil
}

// TestWorkerSummaryCatalogRetainsEarlierCommits pins the compaction window:
// every commit compacts the complete conversation, so the active summary
// manifest keeps the records of earlier commits instead of replacing them
// with the records of the latest commit only.
func TestWorkerSummaryCatalogRetainsEarlierCommits(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"chunk_size": 1, "condense_threshold": 2, "group_size": 2, "max_depth": 2},
	  "interval": "0"
	}`
	assembly, _ := newTestAssemblyWith(t, settings, WithDeriver(perInputDeriver{}))
	ctx := context.Background()
	for index, text := range []string{"the user drinks green tea", "the user also likes coffee"} {
		turn := corememory.Turn{
			Scope: testScope(), ConversationID: "conv-1",
			IdempotencyKey: fmt.Sprintf("run-%d", index+1),
			Messages:       []coremessage.Message{textMessage(coremessage.RoleUser, text)},
		}
		if err := assembly.CommitTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
		if err := assembly.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil || len(facts) != 2 {
		t.Fatalf("facts = %#v, %v", facts, err)
	}
	summaries, err := assembly.Summaries().ListActive(ctx, testScope(), "conv-1", summaryview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]struct{})
	for _, record := range summaries {
		if record.Level != summaryview.L0 {
			continue
		}
		for _, id := range record.InputIDs {
			covered[id] = struct{}{}
		}
	}
	for _, fact := range facts {
		if _, ok := covered[fact.ID]; !ok {
			t.Fatalf("fact %q is missing from the active summary catalog: %#v", fact.ID, summaries)
		}
	}
}

func TestVectorLaneCalibrationRecall(t *testing.T) {
	fake := &inferencetest.EmbedFake{Respond: func(request inference.EmbedRequest) inference.EmbedResponse {
		embeddings := make([]inference.Embedding, len(request.Items))
		for index, item := range request.Items {
			if strings.Contains(strings.ToLower(item.Content.Text()), "tea") {
				embeddings[index] = inference.Embedding{Vector: []float32{1, 0}}
			} else {
				embeddings[index] = inference.Embedding{Vector: []float32{0, 1}}
			}
		}
		return inference.EmbedResponse{Embeddings: embeddings}
	}}
	engine := fake.Assembly(t)
	settings := fmt.Sprintf(`{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "embed": {"provider": %q, "name": %q, "profile": %q},
	  "interval": "0"
	}`, inferencetest.DefaultFakeEmbedModel.ID.Provider, inferencetest.DefaultFakeEmbedModel.ID.Name,
		inferencetest.DefaultFakeEmbedModel.Profile)
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	value, err := NewFactory(
		WithClock(func() time.Time { return testNow }),
		WithDeriver(fakeDeriver{}),
	).New(context.Background(), resource.Input{
		Settings: []byte(settings),
		Deps:     map[string]any{"workspace": ws, "inference": engine},
	})
	if err != nil {
		t.Fatal(err)
	}
	assembly := value.(*Assembly)
	if err := assembly.Wire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "tea",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		if item.Content.Text() == "Alice likes tea" && item.SourceClass == corememory.ContextSourceLongTerm {
			found = true
		}
	}
	if !found {
		t.Fatalf("vector lane did not recall the tea fact: %#v", result.Items)
	}
	if len(fake.Requests()) == 0 {
		t.Fatal("embed model was never called")
	}
}

func TestDiagnosticsReportCursors(t *testing.T) {
	assembly, _ := newTestAssembly(t, "")
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "hello")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Worker.CommitsProcessed != 1 {
		t.Fatalf("commits processed = %d, want 1", diagnostics.Worker.CommitsProcessed)
	}
	if len(diagnostics.Scopes) != 1 || len(diagnostics.Scopes[0].Conversations) != 1 {
		t.Fatalf("scope diagnostics = %#v", diagnostics.Scopes)
	}
	if diagnostics.Scopes[0].Conversations[0].Behind {
		t.Fatal("conversation reported behind after RunOnce")
	}
	second := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-2",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "second")},
	}
	if err := assembly.CommitTurn(ctx, second); err != nil {
		t.Fatal(err)
	}
	diagnostics, err = assembly.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !diagnostics.Scopes[0].Conversations[0].Behind {
		t.Fatal("pending commit was not reported behind")
	}
}

func TestSQLiteStorageDrivers(t *testing.T) {
	database := fmt.Sprintf("%s/memory.db", t.TempDir())
	settings := fmt.Sprintf(`{
	  "storage": {
	    "log": {"driver": "sqlite", "settings": {"path": %q}},
	    "kv": {"driver": "sqlite", "settings": {"path": %q}}
	  },
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "interval": "0"
	}`, database, database)
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	build := func() *Assembly {
		value, err := NewFactory(
			WithClock(func() time.Time { return testNow }),
			WithDeriver(fakeDeriver{}),
		).New(context.Background(), resource.Input{
			Settings: []byte(settings),
			Deps:     map[string]any{"workspace": ws},
		})
		if err != nil {
			t.Fatal(err)
		}
		assembly := value.(*Assembly)
		if err := assembly.Wire(context.Background()); err != nil {
			t.Fatal(err)
		}
		return assembly
	}
	assembly := build()
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := assembly.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := build()
	defer func() { _ = reopened.Close() }()
	result, err := reopened.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "tea",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		if item.Content.Text() == "Alice likes tea" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sqlite-backed recall missing fact: %#v", result.Items)
	}
}

func TestVerifyReportsHealthyState(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"chunk_size": 1, "condense_threshold": 2, "group_size": 2, "max_depth": 2},
	  "interval": "0"
	}`
	assembly, _ := newTestAssemblyWith(t, settings, WithDeriver(fakeDeriver{}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.Verify(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("healthy state reported actions = %#v", plan.Actions)
	}
}

// contradictionDeriver emits one fact per commit with a controlled text,
// entity, and event time, so maintenance tests can build a deterministic
// contradiction history.
type contradictionDeriver struct {
	calls int
	texts []string
	times []time.Time
}

func (deriver *contradictionDeriver) Derive(_ context.Context, input component.Artifact) ([]component.Artifact, error) {
	index := deriver.calls
	deriver.calls++
	if index >= len(deriver.texts) {
		index = len(deriver.texts) - 1
	}
	text := deriver.texts[index]
	hash := factview.CanonicalHash(text)
	return []component.Artifact{{
		Kind:    chat.KindFact,
		ID:      "fact-" + strings.ReplaceAll(hash, ":", "-"),
		Content: coremessage.NewTextContent(text),
		Sources: append([]corememory.SourceRef(nil), input.Sources...),
		Metadata: corememory.Metadata{
			"canonical_hash":      hash,
			"entities":            `["Caroline"]`,
			"event_time":          deriver.times[index].UTC().Format(time.RFC3339Nano),
			"source_digest":       factview.ComputeSourceDigest(input.Sources),
			"transform_signature": "contradiction-derive-v1",
		},
	}}, nil
}

// TestMaintainSoftMergesContradictingFacts is the contradiction fixture: a
// newer fact about the same entity supersedes the older one, and the read
// path halves the older fact's score without editing canonical text.
func TestMaintainSoftMergesContradictingFacts(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "interval": "0"
	}`
	deriver := &contradictionDeriver{
		texts: []string{
			"Caroline works at Acme Corporation since 2023.",
			"Caroline works at Beta Corporation since 2024.",
		},
		// Both facts are fresh, so the fixture isolates soft merge from decay
		// (decay has its own unit test).
		times: []time.Time{testNow.Add(-time.Hour), testNow},
	}
	assembly, _ := newTestAssemblyWith(t, settings, WithDeriver(deriver))
	ctx := context.Background()
	for index, text := range deriver.texts {
		turn := corememory.Turn{
			Scope: testScope(), ConversationID: "conv-1",
			IdempotencyKey: fmt.Sprintf("run-%d", index+1),
			Messages:       []coremessage.Message{textMessage(coremessage.RoleUser, text)},
		}
		if err := assembly.CommitTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	query := func() corememory.ContextResult {
		result, err := assembly.Context(ctx, corememory.ContextRequest{
			Scope: testScope(), ConversationID: "conv-1", Query: "Where does Caroline work?",
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	scoreOf := func(result corememory.ContextResult, needle string) float64 {
		for _, item := range result.Items {
			if item.Kind == corememory.ContextFact && strings.Contains(item.Content.Text(), needle) {
				return item.Score
			}
		}
		return 0
	}
	before := query()
	oldBefore := scoreOf(before, "Acme")
	if oldBefore <= 0 {
		t.Fatalf("older fact missing before maintenance: %#v", before.Items)
	}
	plan, err := assembly.Maintain(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 || len(plan.Decays) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
	after := query()
	oldAfter := scoreOf(after, "Acme")
	if oldAfter <= 0 {
		t.Fatalf("older fact missing after maintenance: %#v", after.Items)
	}
	if math.Abs(oldAfter-oldBefore*0.5) > 1e-9 {
		t.Fatalf("superseded fact score = %v, want %v", oldAfter, oldBefore*0.5)
	}
	if newScore := scoreOf(after, "Beta"); newScore <= 0 {
		t.Fatalf("newer fact missing after maintenance: %#v", after.Items)
	}
}

func TestVerifyReportsDanglingFactLink(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "interval": "0"
	}`
	assembly, _ := newTestAssemblyWith(t, settings, WithDeriver(fakeDeriver{linkedIDs: []string{"missing-fact"}}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.Verify(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, action := range plan.Actions {
		if action.Kind == verify.ActionReplay && strings.Contains(action.Evidence, "dangling") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dangling link not reported: %#v", plan.Actions)
	}
}

func TestDeriveAndHybridContext(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"chunk_size": 1, "condense_threshold": 2, "group_size": 2, "max_depth": 2},
	  "interval": "0"
	}`
	assembly, _ := newTestAssemblyWith(t, settings, WithDeriver(fakeDeriver{}))
	ctx := context.Background()
	turn := corememory.Turn{
		Scope: testScope(), ConversationID: "conv-1", IdempotencyKey: "run-1",
		Messages: []coremessage.Message{textMessage(coremessage.RoleUser, "we talked about beverages")},
	}
	if err := assembly.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Content.Text() != "Alice likes tea" {
		t.Fatalf("facts = %#v", facts)
	}
	summaries, err := assembly.Summaries().ListActive(ctx, testScope(), "conv-1", summaryview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) == 0 {
		t.Fatal("expected at least one active summary")
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: "tea",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	foundSummary := false
	for _, item := range result.Items {
		if item.Content.Text() == "Alice likes tea" && item.SourceClass == corememory.ContextSourceLongTerm {
			found = true
		}
		if item.SourceClass == corememory.ContextSourceSummary {
			foundSummary = true
		}
	}
	if !found {
		t.Fatalf("hybrid context items = %#v", result.Items)
	}
	if !foundSummary {
		t.Fatalf("summary lane missing from context items = %#v", result.Items)
	}
	// A second scan resumes from the watermark and must not duplicate facts.
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	facts, err = assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts after rescan = %d, want 1", len(facts))
	}
}

func TestDocumentKnowledgeRetrieval(t *testing.T) {
	settings := `{
	  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
	  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
	  "fact": {"strategy": "none"},
	  "summary": {"disabled": true},
	  "chunk": {"max_runes": 64, "overlap_runes": 8},
	  "interval": "0"
	}`
	assembly, _ := newTestAssemblyWith(t, settings)
	ctx := context.Background()
	document := corememory.Document{
		Scope: testScope(), DatasetID: "docs", DocumentID: "doc-1", IdempotencyKey: "rev-1",
		Content: coremessage.Content{Parts: []coremessage.Part{
			coremessage.TextPart{Text: "The quarterly report covers espresso brewing techniques in detail."},
		}},
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceExternal, ID: "uri:report"}},
	}
	if err := assembly.PutDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	if err := assembly.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := assembly.Context(ctx, corememory.ContextRequest{
		Scope: testScope(), Query: "espresso", DatasetIDs: []string{"docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Items {
		if item.Kind == corememory.ContextDocumentChunk && strings.Contains(item.Content.Text(), "espresso") {
			found = true
		}
	}
	if !found {
		t.Fatalf("document chunk missing from context items = %#v", result.Items)
	}
}
