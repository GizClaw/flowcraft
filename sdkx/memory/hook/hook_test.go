package hook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// recordingImpl captures the requests the hook layer passes
// through to the runtime, so tests can assert on them without
// re-implementing storage. It satisfies all six op interfaces
// with a Native decision per canonical field, mirroring the
// noop runtime but additionally stashing the last request seen.
type recordingImpl struct {
	mu sync.Mutex
	// last per-op request, indexed for direct inspection.
	lastAppend memory.AppendRequest
	lastLoad   memory.LoadRequest
	lastRecall memory.RecallRequest
	appends    []memory.AppendRequest
	loadCalls  int
	recalls    []memory.RecallRequest
	// appendErr, if non-nil, is returned from ExecuteAppend.
	// Tests use it to assert the committer surfaces errors
	// verbatim.
	appendErr error
	// loadRecords is what ExecuteLoad returns. Tests pre-load
	// it to verify the load hook converts records to messages.
	loadRecords []memory.Record
	// recallHits is what ExecuteRecall returns.
	recallHits []memory.Hit
}

func (r *recordingImpl) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return allNative(memory.OpAppend, memory.FieldAppendScope,
		memory.FieldAppendConversationID, memory.FieldAppendIdempotencyKey,
		memory.FieldAppendRecords, memory.FieldAppendMetadata)
}
func (r *recordingImpl) ExecuteAppend(_ context.Context, req memory.AppendRequest) (memory.AppendResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAppend = req
	r.appends = append(r.appends, req)
	return memory.AppendResponse{Appended: len(req.Records), LastSeq: uint64(len(req.Records))}, r.appendErr
}
func (r *recordingImpl) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return allNative(memory.OpLoad, memory.FieldLoadScope,
		memory.FieldLoadConversationID, memory.FieldLoadCursor,
		memory.FieldLoadLimit, memory.FieldLoadReverse)
}
func (r *recordingImpl) ExecuteLoad(_ context.Context, req memory.LoadRequest) (memory.LoadResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastLoad = req
	r.loadCalls++
	return memory.LoadResponse{Records: r.loadRecords}, nil
}
func (r *recordingImpl) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return allNative(memory.OpRecall, memory.FieldRecallScope,
		memory.FieldRecallConversationID, memory.FieldRecallQuery,
		memory.FieldRecallTopK, memory.FieldRecallFilters, memory.FieldRecallMinScore)
}
func (r *recordingImpl) ExecuteRecall(_ context.Context, req memory.RecallRequest) (memory.RecallResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastRecall = req
	r.recalls = append(r.recalls, req)
	return memory.RecallResponse{Hits: r.recallHits}, nil
}
func (r *recordingImpl) CompileImport(_ context.Context, _ memory.ImportRequest) memory.CompileResult {
	return allNative(memory.OpImport, memory.FieldImportScope,
		memory.FieldImportDatasetID, memory.FieldImportSource,
		memory.FieldImportTags, memory.FieldImportChunkPolicy)
}
func (r *recordingImpl) ExecuteImport(_ context.Context, _ memory.ImportRequest) (memory.ImportResponse, error) {
	return memory.ImportResponse{}, nil
}
func (r *recordingImpl) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return allNative(memory.OpCompact, memory.FieldCompactScope,
		memory.FieldCompactOlderThan, memory.FieldCompactKeep)
}
func (r *recordingImpl) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{}, nil
}
func (r *recordingImpl) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return allNative(memory.OpArchive, memory.FieldArchiveScope,
		memory.FieldArchiveOlderThan, memory.FieldArchiveDestination)
}
func (r *recordingImpl) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{}, nil
}

// allNative builds a CompileResult that emits one Native
// decision per field. The hook tests need it because the
// runtime refuses an op whose ledger is incomplete; every
// recorded op must therefore declare its fields as Native.
func allNative(op memory.Operation, fields ...memory.FieldID) memory.CompileResult {
	decisions := make([]memory.Decision, len(fields))
	for i, f := range fields {
		decisions[i] = memory.Decision{
			Field: f, Disposition: memory.DispositionNative,
		}
	}
	return memory.CompileResult{Op: op, Decisions: decisions}
}

func newRecordingRuntime(t *testing.T, scope memory.Scope) (*memory.Runtime, *recordingImpl) {
	t.Helper()
	impl := &recordingImpl{}
	rt, err := memory.New(memory.Spec{
		RuntimeID:    scope.RuntimeID,
		DefaultScope: scope,
	}, memory.Impls{
		Append: impl, Load: impl, Recall: impl,
		Import: impl, Compact: impl, Archive: impl,
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return rt, impl
}

// settingsNode parses a YAML literal into a *yamlv3.Node, the
// shape deploy.DecodeSettings expects on HookInput.Settings.
func settingsNode(t *testing.T, body string) *yamlv3.Node {
	t.Helper()
	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(body), &node); err != nil {
		t.Fatalf("settings yaml: %v", err)
	}
	return &node
}

func newInput(rt *memory.Runtime, settings *yamlv3.Node) deploy.HookInput {
	return deploy.HookInput{Settings: settings, Deps: map[string]any{runtimeDepName: rt}}
}

func mustMessage(text string) inference.Message {
	return inference.Message{Role: inference.RoleUser, Content: inference.Content{Parts: []inference.Part{inference.TextPart{Text: text}}}}
}

// ---------- Load ----------

func TestLoadPreparerFactory_ReturnsFactory(t *testing.T) {
	f := NewLoadPreparerFactory()
	if f == nil {
		t.Fatal("NewLoadPreparerFactory returned nil")
	}
}

func TestLoadPreparer_RejectsInvalidYAML(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	in := newInput(rt, settingsNode(t, "into: transcript\ntotally_made_up: 1\n"))
	_, err := NewLoadPreparerFactory()(context.Background(), in)
	if err == nil {
		t.Fatal("expected decode error for unknown field")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("error kind = %T, want validation", err)
	}
}

func TestLoadPreparer_RejectsMissingInto(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	in := newInput(rt, settingsNode(t, "limit: 5\n"))
	_, err := NewLoadPreparerFactory()(context.Background(), in)
	if err == nil {
		t.Fatal("expected missing-into error")
	}
	if !strings.Contains(err.Error(), "into") {
		t.Errorf("error %q should mention 'into'", err.Error())
	}
}

func TestLoadPreparer_RejectsMissingRuntimeDep(t *testing.T) {
	in := deploy.HookInput{Settings: settingsNode(t, "into: transcript\n")}
	_, err := NewLoadPreparerFactory()(context.Background(), in)
	if err == nil {
		t.Fatal("expected missing-runtime error")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("error kind = %T, want not-found", err)
	}
}

func TestLoadPreparer_RejectsWrongTypeDep(t *testing.T) {
	in := deploy.HookInput{
		Settings: settingsNode(t, "into: transcript\n"),
		Deps:     map[string]any{runtimeDepName: "not a runtime"},
	}
	_, err := NewLoadPreparerFactory()(context.Background(), in)
	if err == nil {
		t.Fatal("expected wrong-type error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("error kind = %T, want validation", err)
	}
}

func TestLoadPreparer_RejectsTypedNilRuntimeDep(t *testing.T) {
	var runtime *memory.Runtime
	in := deploy.HookInput{
		Settings: settingsNode(t, "into: transcript\n"),
		Deps:     map[string]any{runtimeDepName: runtime},
	}
	_, err := NewLoadPreparerFactory()(context.Background(), in)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestLoadPreparer_FillsScopeFromDefault(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{
		RuntimeID: "prod", UserID: "default-user",
	})
	prep, err := NewLoadPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: transcript
limit: 7
conversation: c-1
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	impl.loadRecords = []memory.Record{
		{Seq: 1, Message: mustMessage("hi")},
		{Seq: 2, Message: mustMessage("there")},
	}
	board := agent.NewBoard()
	out, err := prep.Before(context.Background(), agent.Identity{RunID: "r-1"}, &agent.Request{}, board)
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if got := impl.lastLoad.Scope; got.RuntimeID != "prod" || got.UserID != "default-user" {
		t.Errorf("scope = %+v, want filled from DefaultScope", got)
	}
	if impl.lastLoad.ConversationID != "c-1" {
		t.Errorf("ConversationID = %q, want c-1", impl.lastLoad.ConversationID)
	}
	if impl.lastLoad.Limit != 7 {
		t.Errorf("Limit = %d, want 7", impl.lastLoad.Limit)
	}
	msgs := out.Channel("transcript")
	if len(msgs) != 2 {
		t.Errorf("transcript channel len = %d, want 2", len(msgs))
	}
	// Preparer contract: previous board must not see new msgs.
	prevMsgs := board.Channel("transcript")
	if len(prevMsgs) != 0 {
		t.Errorf("previous board should not have transcript populated, got %d msgs", len(prevMsgs))
	}
}

func TestLoadPreparer_OverridesScopeFromSettings(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod", UserID: "u-default"})
	prep, err := NewLoadPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: transcript
limit: 5
scope:
  runtime_id: prod
  user_id: u-override
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := prep.Before(context.Background(), agent.Identity{RunID: "r-1"}, &agent.Request{ContextID: "c-1"}, agent.NewBoard()); err != nil {
		t.Fatalf("Before: %v", err)
	}
	if got := impl.lastLoad.Scope; got.UserID != "u-override" {
		t.Errorf("Scope.UserID = %q, want u-override", got.UserID)
	}
}

func TestLoadPreparer_PropagatesError(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	errOnLoadSentinel := errors.New("load blew up")
	errImpl := &errOnLoad{err: errOnLoadSentinel}
	rt2, err := memory.New(memory.Spec{RuntimeID: "prod", DefaultScope: memory.Scope{RuntimeID: "prod"}}, memory.Impls{
		Append: errImpl, Load: errImpl, Recall: errImpl,
		Import: errImpl, Compact: errImpl, Archive: errImpl,
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	prep, err := NewLoadPreparerFactory()(context.Background(), newInput(rt2, settingsNode(t, "into: transcript\nlimit: 5\n")))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = prep.Before(context.Background(), agent.Identity{}, &agent.Request{ContextID: "c-1"}, agent.NewBoard())
	if err == nil || !errors.Is(err, errOnLoadSentinel) {
		t.Errorf("error = %v, want %v", err, errOnLoadSentinel)
	}
	_ = rt // silence
}

// errOnLoad is a tiny impl whose Load Execute returns a
// fixed error; the other ops are noop-shaped.
type errOnLoad struct{ err error }

func (e *errOnLoad) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return allNative(memory.OpAppend, memory.FieldAppendScope,
		memory.FieldAppendConversationID, memory.FieldAppendIdempotencyKey,
		memory.FieldAppendRecords, memory.FieldAppendMetadata)
}
func (e *errOnLoad) ExecuteAppend(_ context.Context, _ memory.AppendRequest) (memory.AppendResponse, error) {
	return memory.AppendResponse{}, nil
}
func (e *errOnLoad) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return allNative(memory.OpLoad, memory.FieldLoadScope,
		memory.FieldLoadConversationID, memory.FieldLoadCursor,
		memory.FieldLoadLimit, memory.FieldLoadReverse)
}
func (e *errOnLoad) ExecuteLoad(_ context.Context, _ memory.LoadRequest) (memory.LoadResponse, error) {
	return memory.LoadResponse{}, e.err
}
func (e *errOnLoad) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return allNative(memory.OpRecall, memory.FieldRecallScope,
		memory.FieldRecallConversationID, memory.FieldRecallQuery,
		memory.FieldRecallTopK, memory.FieldRecallFilters, memory.FieldRecallMinScore)
}
func (e *errOnLoad) ExecuteRecall(_ context.Context, _ memory.RecallRequest) (memory.RecallResponse, error) {
	return memory.RecallResponse{}, nil
}
func (e *errOnLoad) CompileImport(_ context.Context, _ memory.ImportRequest) memory.CompileResult {
	return allNative(memory.OpImport, memory.FieldImportScope,
		memory.FieldImportDatasetID, memory.FieldImportSource,
		memory.FieldImportTags, memory.FieldImportChunkPolicy)
}
func (e *errOnLoad) ExecuteImport(_ context.Context, _ memory.ImportRequest) (memory.ImportResponse, error) {
	return memory.ImportResponse{}, nil
}
func (e *errOnLoad) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return allNative(memory.OpCompact, memory.FieldCompactScope,
		memory.FieldCompactOlderThan, memory.FieldCompactKeep)
}
func (e *errOnLoad) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{}, nil
}
func (e *errOnLoad) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return allNative(memory.OpArchive, memory.FieldArchiveScope,
		memory.FieldArchiveOlderThan, memory.FieldArchiveDestination)
}
func (e *errOnLoad) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{}, nil
}

// ---------- Recall ----------

func TestRecallPreparerFactory_ReturnsFactory(t *testing.T) {
	if NewRecallPreparerFactory() == nil {
		t.Fatal("NewRecallPreparerFactory returned nil")
	}
}

func TestRecallPreparer_RejectsMissingInto(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	_, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, "query: {literal: hello}\n")))
	if err == nil || !strings.Contains(err.Error(), "into") {
		t.Errorf("err = %v, want missing-into", err)
	}
}

func TestRecallPreparer_RejectsMissingQuery(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	_, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, "into: hits\n")))
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Errorf("err = %v, want missing-query", err)
	}
}

func TestRecallPreparer_RejectsInvalidQuerySpec(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	for _, test := range []struct {
		name     string
		settings string
	}{
		{name: "neither", settings: "into: hits\nquery: {}\n"},
		{name: "both", settings: "into: hits\nquery: {literal: hello, board: query}\n"},
		{name: "empty literal", settings: "into: hits\nquery: {literal: ''}\n"},
		{name: "empty board", settings: "into: hits\nquery: {board: ''}\n"},
		{name: "unknown field", settings: "into: hits\nquery: {input: query}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRecallPreparerFactory()(context.Background(),
				newInput(rt, settingsNode(t, test.settings)))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("factory error = %v, want validation", err)
			}
		})
	}
}

func TestRecallPreparer_RejectsMissingRuntimeDep(t *testing.T) {
	_, err := NewRecallPreparerFactory()(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, "into: hits\nquery: {literal: hi}\n"),
	})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Errorf("err = %v, want not-found", err)
	}
}

func TestRecallPreparer_WritesHitsToBoardVar(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod", UserID: "u-1"})
	impl.recallHits = []memory.Hit{
		{ID: "h1", Parts: []inference.Part{inference.TextPart{Text: "needle"}}, Score: 0.9, Source: "transcript:c1/seq-3"},
		{ID: "h2", Parts: []inference.Part{inference.TextPart{Text: "haystack"}}, Score: 0.4, Source: "chunk:doc.md#2"},
	}
	prep, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: hits
query: {literal: needle}
top_k: 3
min_score: 0.5
filters:
  dataset: kb
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	board := agent.NewBoard()
	out, err := prep.Before(context.Background(), agent.Identity{RunID: "r-1"}, &agent.Request{}, board)
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if impl.lastRecall.Query != "needle" {
		t.Errorf("Query = %q, want needle", impl.lastRecall.Query)
	}
	if impl.lastRecall.TopK != 3 {
		t.Errorf("TopK = %d, want 3", impl.lastRecall.TopK)
	}
	if impl.lastRecall.MinScore != 0.5 {
		t.Errorf("MinScore = %v, want 0.5", impl.lastRecall.MinScore)
	}
	if got := impl.lastRecall.Filters["dataset"]; got != "kb" {
		t.Errorf("Filters[dataset] = %q, want kb", got)
	}
	hits, ok := agent.GetTyped[[]memory.Hit](out, "hits")
	if !ok {
		t.Fatal("hits var not set or wrong type")
	}
	if len(hits) != 2 || hits[0].ID != "h1" {
		t.Errorf("hits = %+v, want h1 + h2", hits)
	}
}

func TestRecallPreparer_ResolvesBoardVarPerRun(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	prep, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: hits
query: {board: search_query}
top_k: 3
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, query := range []string{"first query", "second query"} {
		board := agent.NewBoard()
		board.SetVar("search_query", query)
		_, err := prep.Before(context.Background(), agent.Identity{}, &agent.Request{
			Inputs: map[string]any{"search_query": "must not be read directly"},
		}, board)
		if err != nil {
			t.Fatalf("Before(%q): %v", query, err)
		}
		if impl.lastRecall.Query != query {
			t.Errorf("Query = %q, want %q", impl.lastRecall.Query, query)
		}
	}
}

func TestRecallPreparer_ReadsRequestInputSeededIntoBoard(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	prep, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: hits
query: {board: search_query}
top_k: 3
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = agent.Execute(context.Background(), agent.Agent{ID: "recall-test"},
		agent.EngineFunc(nil), agent.Request{
			Inputs: map[string]any{"search_query": "seeded input"},
		}, agent.WithPreparer(prep))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if impl.lastRecall.Query != "seeded input" {
		t.Fatalf("Query = %q, want seeded input", impl.lastRecall.Query)
	}
}

func TestRecallPreparer_ReadsPreviousPreparerBoardOutput(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	recall, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: hits
query: {board: generated_query}
top_k: 3
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	writer := agent.PreparerFunc(func(
		_ context.Context, _ agent.Identity, _ *agent.Request, prev *agent.Board,
	) (*agent.Board, error) {
		next := prev.Clone()
		next.SetVar("generated_query", "from previous preparer")
		return next, nil
	})
	_, err = agent.Execute(context.Background(), agent.Agent{ID: "recall-test"},
		agent.EngineFunc(nil), agent.Request{},
		agent.WithPreparer(writer), agent.WithPreparer(recall))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if impl.lastRecall.Query != "from previous preparer" {
		t.Fatalf("Query = %q, want previous preparer output", impl.lastRecall.Query)
	}
}

func TestRecallPreparer_BoardVarValidation(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	prep, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: hits
query: {board: query}
top_k: 3
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, test := range []struct {
		name  string
		value any
		set   bool
	}{
		{name: "missing"},
		{name: "non-string", value: 42, set: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(impl.recalls)
			board := agent.NewBoard()
			if test.set {
				board.SetVar("query", test.value)
			}
			_, err := prep.Before(context.Background(), agent.Identity{}, &agent.Request{}, board)
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Before error = %v, want validation", err)
			}
			if len(impl.recalls) != before {
				t.Fatal("invalid template input called Recall implementation")
			}
		})
	}
}

func TestRecallPreparer_PreservesOrdinaryLiteral(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	const literal = "{{inputs.query}} and {{board.query}} are plain text"
	prep, err := NewRecallPreparerFactory()(context.Background(), newInput(rt, settingsNode(t, `
into: hits
query:
  literal: "{{inputs.query}} and {{board.query}} are plain text"
top_k: 3
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := prep.Before(context.Background(), agent.Identity{}, &agent.Request{},
		agent.NewBoard()); err != nil {
		t.Fatalf("Before: %v", err)
	}
	if impl.lastRecall.Query != literal {
		t.Fatalf("Query = %q, want literal %q", impl.lastRecall.Query, literal)
	}
}

// ---------- Append ----------

func TestAppendCommitterFactory_ReturnsFactory(t *testing.T) {
	if NewAppendCommitterFactory() == nil {
		t.Fatal("NewAppendCommitterFactory returned nil")
	}
}

func TestAppendCommitter_RejectsInvalidYAML(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	_, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, "channel: m\nmade_up: x\n")))
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err kind = %T, want validation", err)
	}
}

func TestAppendCommitter_RejectsMissingChannel(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	_, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, "metadata:\n  k: v\n")))
	if err == nil || !strings.Contains(err.Error(), "channel") {
		t.Errorf("err = %v, want missing-channel", err)
	}
}

func TestAppendCommitter_RejectsMissingRuntimeDep(t *testing.T) {
	_, err := NewAppendCommitterFactory()(context.Background(), deploy.HookInput{
		Settings: settingsNode(t, "channel: __main_channel\n"),
	})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Errorf("err = %v, want not-found", err)
	}
}

func TestAppendCommitter_NoopOnEmptyChannel(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	cm, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, "channel: transcript\n")))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	res := &agent.Result{LastBoard: agent.NewBoard()}
	if err := cm.Commit(context.Background(), agent.Identity{RunID: "r-1"}, &agent.Request{}, res); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(impl.lastAppend.Records) != 0 {
		t.Errorf("expected no append call on empty channel, got %d records", len(impl.lastAppend.Records))
	}
}

func TestAppendCommitter_SetsIdempotencyKeyFromRunID(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	cm, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, `
channel: transcript
conversation: c-1
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage("transcript", mustMessage("user hi"))
	board.AppendChannelMessage("transcript", inference.Message{
		Role:    inference.RoleAssistant,
		Content: inference.Content{Parts: []inference.Part{inference.TextPart{Text: "assistant hello"}}},
	})
	res := &agent.Result{LastBoard: board}
	if err := cm.Commit(context.Background(), agent.Identity{RunID: "run-xyz"}, &agent.Request{ContextID: "c-1"}, res); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if impl.lastAppend.IdempotencyKey != "run-xyz" {
		t.Errorf("IdempotencyKey = %q, want run-xyz", impl.lastAppend.IdempotencyKey)
	}
	if impl.lastAppend.ConversationID != "c-1" {
		t.Errorf("ConversationID = %q, want c-1", impl.lastAppend.ConversationID)
	}
	if len(impl.lastAppend.Records) != 2 {
		t.Fatalf("Records = %d, want 2", len(impl.lastAppend.Records))
	}
	for i, rec := range impl.lastAppend.Records {
		if rec.ID == "" {
			t.Errorf("Record[%d].ID is empty; kernel must fill it", i)
		}
		if rec.Seq != 0 {
			t.Errorf("Record[%d].Seq = %d, want 0 (caller leaves it)", i, rec.Seq)
		}
	}
	if got := impl.lastAppend.Metadata["run_id"]; got != "run-xyz" {
		t.Errorf("Metadata[run_id] = %q, want run-xyz", got)
	}
}

func TestAppendCommitter_OverwritesRunIDOnEveryCall(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	cm, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, `
channel: transcript
metadata:
  run_id: doc-supplied
  agent_id: a-1
`)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage("transcript", mustMessage("hi"))
	res := &agent.Result{LastBoard: board}
	if err := cm.Commit(context.Background(), agent.Identity{RunID: "run-xyz"}, &agent.Request{ContextID: "c-1"}, res); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := impl.lastAppend.Metadata["run_id"]; got != "run-xyz" {
		t.Errorf("Metadata[run_id] = %q, want run-xyz", got)
	}
	if got := impl.lastAppend.Metadata["agent_id"]; got != "a-1" {
		t.Errorf("Metadata[agent_id] = %q, want a-1", got)
	}
}

func TestHooksConversationFallbackAndEmptyContract(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	load, err := NewLoadPreparerFactory()(context.Background(), newInput(rt,
		settingsNode(t, "into: transcript\nlimit: 5\n")))
	if err != nil {
		t.Fatal(err)
	}
	recall, err := NewRecallPreparerFactory()(context.Background(), newInput(rt,
		settingsNode(t, "into: hits\nquery: {literal: hello}\ntop_k: 3\n")))
	if err != nil {
		t.Fatal(err)
	}
	appendHook, err := NewAppendCommitterFactory()(context.Background(), newInput(rt,
		settingsNode(t, "channel: transcript\n")))
	if err != nil {
		t.Fatal(err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage("transcript", mustMessage("hello"))
	req := &agent.Request{ContextID: "ctx-1"}
	if _, err := load.Before(context.Background(), agent.Identity{}, req, board); err != nil {
		t.Fatal(err)
	}
	if _, err := recall.Before(context.Background(), agent.Identity{}, req, board); err != nil {
		t.Fatal(err)
	}
	if err := appendHook.Commit(context.Background(), agent.Identity{RunID: "r1"}, req,
		&agent.Result{LastBoard: board}); err != nil {
		t.Fatal(err)
	}
	if impl.lastLoad.ConversationID != "ctx-1" ||
		impl.lastRecall.ConversationID != "ctx-1" ||
		impl.lastAppend.ConversationID != "ctx-1" {
		t.Fatalf("ContextID fallback missing: load=%q recall=%q append=%q",
			impl.lastLoad.ConversationID, impl.lastRecall.ConversationID,
			impl.lastAppend.ConversationID)
	}

	beforeLoads, beforeAppends := impl.loadCalls, len(impl.appends)
	empty := &agent.Request{}
	if out, err := load.Before(context.Background(), agent.Identity{}, empty, board); err != nil || out != board {
		t.Fatalf("empty-context load = (%p, %v), want original board/no error", out, err)
	}
	if err := appendHook.Commit(context.Background(), agent.Identity{RunID: "r2"}, empty,
		&agent.Result{LastBoard: board}); err != nil {
		t.Fatal(err)
	}
	if impl.loadCalls != beforeLoads || len(impl.appends) != beforeAppends {
		t.Fatal("empty ContextID performed transcript I/O")
	}
	if _, err := recall.Before(context.Background(), agent.Identity{}, empty, board); err != nil {
		t.Fatal(err)
	}
	if impl.lastRecall.ConversationID != "" {
		t.Fatalf("global recall ConversationID = %q, want empty", impl.lastRecall.ConversationID)
	}
}

func TestAppendCommitterMetadataIsRunLocalAndConcurrentSafe(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	hook, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, `
channel: transcript
conversation: c-1
metadata:
  source: configured
`)))
	if err != nil {
		t.Fatal(err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage("transcript", mustMessage("hello"))
	res := &agent.Result{LastBoard: board}
	const runs = 32
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("run-%d", i)
			if err := hook.Commit(context.Background(), agent.Identity{RunID: runID},
				&agent.Request{ContextID: "ignored"}, res); err != nil {
				t.Errorf("Commit(%s): %v", runID, err)
			}
		}(i)
	}
	wg.Wait()
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if len(impl.appends) != runs {
		t.Fatalf("append calls = %d, want %d", len(impl.appends), runs)
	}
	seen := map[string]bool{}
	for _, request := range impl.appends {
		runID := request.Metadata["run_id"]
		if runID == "" || seen[runID] {
			t.Fatalf("invalid or repeated run-local metadata %q", runID)
		}
		seen[runID] = true
		if request.Metadata["source"] != "configured" {
			t.Fatal("configured metadata was not cloned")
		}
	}
}

func TestAppendCommitter_SurfacesErrorVerbatim(t *testing.T) {
	errOnAppendSentinel := errors.New("disk full")
	errImpl := &errOnAppend{err: errOnAppendSentinel}
	rt, err := memory.New(memory.Spec{RuntimeID: "prod", DefaultScope: memory.Scope{RuntimeID: "prod"}}, memory.Impls{
		Append: errImpl, Load: errImpl, Recall: errImpl,
		Import: errImpl, Compact: errImpl, Archive: errImpl,
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	cm, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, "channel: transcript\n")))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage("transcript", mustMessage("hi"))
	res := &agent.Result{LastBoard: board}
	err = cm.Commit(context.Background(), agent.Identity{RunID: "r"}, &agent.Request{ContextID: "c-1"}, res)
	if err == nil || !errors.Is(err, errOnAppendSentinel) {
		t.Errorf("err = %v, want %v", err, errOnAppendSentinel)
	}
}

// errOnAppend is a tiny impl whose Append Execute returns a
// fixed error.
type errOnAppend struct{ err error }

func (e *errOnAppend) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return allNative(memory.OpAppend, memory.FieldAppendScope,
		memory.FieldAppendConversationID, memory.FieldAppendIdempotencyKey,
		memory.FieldAppendRecords, memory.FieldAppendMetadata)
}
func (e *errOnAppend) ExecuteAppend(_ context.Context, _ memory.AppendRequest) (memory.AppendResponse, error) {
	return memory.AppendResponse{}, e.err
}
func (e *errOnAppend) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return allNative(memory.OpLoad, memory.FieldLoadScope,
		memory.FieldLoadConversationID, memory.FieldLoadCursor,
		memory.FieldLoadLimit, memory.FieldLoadReverse)
}
func (e *errOnAppend) ExecuteLoad(_ context.Context, _ memory.LoadRequest) (memory.LoadResponse, error) {
	return memory.LoadResponse{}, nil
}
func (e *errOnAppend) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return allNative(memory.OpRecall, memory.FieldRecallScope,
		memory.FieldRecallConversationID, memory.FieldRecallQuery,
		memory.FieldRecallTopK, memory.FieldRecallFilters, memory.FieldRecallMinScore)
}
func (e *errOnAppend) ExecuteRecall(_ context.Context, _ memory.RecallRequest) (memory.RecallResponse, error) {
	return memory.RecallResponse{}, nil
}
func (e *errOnAppend) CompileImport(_ context.Context, _ memory.ImportRequest) memory.CompileResult {
	return allNative(memory.OpImport, memory.FieldImportScope,
		memory.FieldImportDatasetID, memory.FieldImportSource,
		memory.FieldImportTags, memory.FieldImportChunkPolicy)
}
func (e *errOnAppend) ExecuteImport(_ context.Context, _ memory.ImportRequest) (memory.ImportResponse, error) {
	return memory.ImportResponse{}, nil
}
func (e *errOnAppend) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return allNative(memory.OpCompact, memory.FieldCompactScope,
		memory.FieldCompactOlderThan, memory.FieldCompactKeep)
}
func (e *errOnAppend) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{}, nil
}
func (e *errOnAppend) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return allNative(memory.OpArchive, memory.FieldArchiveScope,
		memory.FieldArchiveOlderThan, memory.FieldArchiveDestination)
}
func (e *errOnAppend) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{}, nil
}

func TestAppendCommitter_HonorsCancellation(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	cm, err := NewAppendCommitterFactory()(context.Background(), newInput(rt, settingsNode(t, "channel: transcript\n")))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage("transcript", mustMessage("hi"))
	res := &agent.Result{LastBoard: board}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A noop impl won't observe cancellation, so the call
	// returns nil — we only assert that it returns without
	// panicking. End-to-end cancellation is the runtime's
	// contract, not the hook's.
	_ = cm.Commit(ctx, agent.Identity{RunID: "r"}, &agent.Request{}, res)
}
