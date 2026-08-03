package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/tool"
	memtool "github.com/GizClaw/flowcraft/sdkx/tool/memory"
)

// recordingImport captures the ImportRequest the tool layer
// passes through, so tests can assert on it. The other five ops
// are noop-shaped so the runtime builds.
type recordingImport struct {
	mu         sync.Mutex
	last       memory.ImportRequest
	resp       memory.ImportResponse
	executeErr error
}

func (r *recordingImport) CompileImport(_ context.Context, req memory.ImportRequest) memory.CompileResult {
	return allNativeImport(memory.OpImport,
		memory.FieldImportScope, memory.FieldImportDatasetID,
		memory.FieldImportSource, memory.FieldImportTags, memory.FieldImportChunkPolicy)
}
func (r *recordingImport) ExecuteImport(_ context.Context, req memory.ImportRequest) (memory.ImportResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = req
	return r.resp, r.executeErr
}
func (r *recordingImport) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return allNativeImport(memory.OpAppend, memory.FieldAppendScope,
		memory.FieldAppendConversationID, memory.FieldAppendIdempotencyKey,
		memory.FieldAppendRecords, memory.FieldAppendMetadata)
}
func (r *recordingImport) ExecuteAppend(_ context.Context, _ memory.AppendRequest) (memory.AppendResponse, error) {
	return memory.AppendResponse{}, nil
}
func (r *recordingImport) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return allNativeImport(memory.OpLoad, memory.FieldLoadScope,
		memory.FieldLoadConversationID, memory.FieldLoadCursor,
		memory.FieldLoadLimit, memory.FieldLoadReverse)
}
func (r *recordingImport) ExecuteLoad(_ context.Context, _ memory.LoadRequest) (memory.LoadResponse, error) {
	return memory.LoadResponse{}, nil
}
func (r *recordingImport) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return allNativeImport(memory.OpRecall, memory.FieldRecallScope,
		memory.FieldRecallConversationID, memory.FieldRecallQuery,
		memory.FieldRecallTopK, memory.FieldRecallFilters, memory.FieldRecallMinScore)
}
func (r *recordingImport) ExecuteRecall(_ context.Context, _ memory.RecallRequest) (memory.RecallResponse, error) {
	return memory.RecallResponse{}, nil
}
func (r *recordingImport) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return allNativeImport(memory.OpCompact, memory.FieldCompactScope,
		memory.FieldCompactOlderThan, memory.FieldCompactKeep)
}
func (r *recordingImport) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{}, nil
}
func (r *recordingImport) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return allNativeImport(memory.OpArchive, memory.FieldArchiveScope,
		memory.FieldArchiveOlderThan, memory.FieldArchiveDestination)
}
func (r *recordingImport) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{}, nil
}

// allNativeImport mirrors memorytest's allNative. We can't import
// memorytest from here (memorytest imports memory, not sdkx), so
// the helper is inlined.
func allNativeImport(op memory.Operation, fields ...memory.FieldID) memory.CompileResult {
	decisions := make([]memory.Decision, len(fields))
	for i, f := range fields {
		decisions[i] = memory.Decision{Field: f, Disposition: memory.DispositionNative}
	}
	return memory.CompileResult{Op: op, Decisions: decisions}
}

func newRecordingRuntime(t *testing.T, scope memory.Scope) (*memory.Runtime, *recordingImport) {
	t.Helper()
	impl := &recordingImport{}
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

func validSettings() memtool.ImportSettings {
	return memtool.ImportSettings{
		Scope:     memtool.ScopeConfig{RuntimeID: "prod"},
		DatasetID: "kb",
		Source:    "/docs/handbook.md",
		Tags:      []string{"team:platform"},
	}
}

func TestImportSettings_Validate(t *testing.T) {
	t.Run("rejects_empty_dataset", func(t *testing.T) {
		s := validSettings()
		s.DatasetID = ""
		if err := s.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("accepts_empty_source_for_per_call_override", func(t *testing.T) {
		s := validSettings()
		s.Source = ""
		if err := s.Validate(); err != nil {
			t.Fatalf("empty default source rejected: %v", err)
		}
	})
	t.Run("accepts_valid", func(t *testing.T) {
		if err := validSettings().Validate(); err != nil {
			t.Errorf("valid settings rejected: %v", err)
		}
	})
}

func TestChunkPolicySnakeCaseYAMLTags(t *testing.T) {
	var policy memory.ChunkPolicy
	if err := yamlv3.Unmarshal([]byte(`
min_chunk_size: 10
max_chunk_size: 100
respect_code: true
`), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.MinChunkSize != 10 || policy.MaxChunkSize != 100 || !policy.RespectCode {
		t.Fatalf("ChunkPolicy = %+v, want snake_case YAML fields", policy)
	}
}

func TestNewImportTool_RejectsNilRuntime(t *testing.T) {
	_, err := memtool.NewImportTool(nil, validSettings())
	if err == nil {
		t.Fatal("expected nil-runtime error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err kind = %T, want validation", err)
	}
}

func TestNewImportTool_RejectsInvalidSettings(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	_, err := memtool.NewImportTool(rt, memtool.ImportSettings{
		// no DatasetID, no Source
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestNewImportTool_ResolvesScopeFromDefault(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod", UserID: "u-default"})
	tl, err := memtool.NewImportTool(rt, memtool.ImportSettings{
		// Scope is empty here; must fill from DefaultScope.
		DatasetID: "kb",
		Source:    "/docs/a.md",
	})
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	impl.resp = memory.ImportResponse{DocumentID: "doc-1", ChunkCount: 7}
	out, err := tl.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := impl.last.Scope; got.RuntimeID != "prod" || got.UserID != "u-default" {
		t.Errorf("scope = %+v, want filled from DefaultScope", got)
	}
	if !strings.Contains(out, `"document_id":"doc-1"`) || !strings.Contains(out, `"chunk_count":7`) {
		t.Errorf("output = %s, want document_id + chunk_count", out)
	}
}

func TestImportTool_Definition(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	tl, err := memtool.NewImportTool(rt, validSettings())
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	def := tl.Definition()
	if def.Name != memtool.ImportToolKind {
		t.Errorf("Name = %q, want %q", def.Name, memtool.ImportToolKind)
	}
	if def.Description == "" {
		t.Error("Description should not be empty")
	}
	// InputSchema must be a JSON object.
	var schema map[string]any
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("InputSchema not a JSON object: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Errorf("schema missing 'properties': %v", schema)
	}
}

func TestImportTool_Execute_UsesConfiguredDefaults(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	tl, err := memtool.NewImportTool(rt, memtool.ImportSettings{
		Scope:     memtool.ScopeConfig{RuntimeID: "prod"},
		DatasetID: "kb",
		Source:    "/docs/default.md",
		Tags:      []string{"configured"},
	})
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	impl.resp = memory.ImportResponse{DocumentID: "doc-x", ChunkCount: 3}
	if _, err := tl.Execute(context.Background(), ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if impl.last.Source != "/docs/default.md" {
		t.Errorf("Source = %q, want /docs/default.md", impl.last.Source)
	}
	if impl.last.DatasetID != "kb" {
		t.Errorf("DatasetID = %q, want kb", impl.last.DatasetID)
	}
	if got := impl.last.Tags; len(got) != 1 || got[0] != "configured" {
		t.Errorf("Tags = %v, want [configured]", got)
	}
}

func TestImportTool_Execute_OverridesFromArguments(t *testing.T) {
	rt, impl := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	tl, err := memtool.NewImportTool(rt, memtool.ImportSettings{
		Scope:     memtool.ScopeConfig{RuntimeID: "prod"},
		DatasetID: "kb",
		Source:    "/docs/default.md",
		Tags:      []string{"configured"},
	})
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	impl.resp = memory.ImportResponse{DocumentID: "doc-y", ChunkCount: 2}
	args := `{"source":"/docs/override.md","tags":["a","b"],"chunk_policy":{"min_chunk_size":10,"max_chunk_size":100,"respect_code":true}}`
	if _, err := tl.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if impl.last.Source != "/docs/override.md" {
		t.Errorf("Source = %q, want /docs/override.md", impl.last.Source)
	}
	if got := impl.last.Tags; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Tags = %v, want [a b]", got)
	}
	// dataset_id not in args → falls back to configured default.
	if impl.last.DatasetID != "kb" {
		t.Errorf("DatasetID = %q, want kb", impl.last.DatasetID)
	}
	if got := impl.last.ChunkPolicy; got.MinChunkSize != 10 ||
		got.MaxChunkSize != 100 || !got.RespectCode {
		t.Errorf("ChunkPolicy = %+v, want snake_case fields decoded", got)
	}
}

func TestImportTool_Execute_RejectsBadJSON(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	tl, err := memtool.NewImportTool(rt, validSettings())
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	_, err = tl.Execute(context.Background(), `{not json`)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("err kind = %T, want validation", err)
	}
}

func TestImportTool_Execute_RequiresSourceAfterMerging(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	settings := validSettings()
	settings.Source = ""
	tl, err := memtool.NewImportTool(rt, settings)
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	if _, err := tl.Execute(context.Background(), `{}`); err == nil ||
		!errdefs.IsValidation(err) {
		t.Fatalf("Execute error = %v, want validation", err)
	}
}

func TestImportTool_Execute_StrictJSONObject(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	tl, err := memtool.NewImportTool(rt, validSettings())
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	for _, args := range []string{
		`null`,
		`[]`,
		`{"unknown":true}`,
		`{"source":"a"} {"source":"b"}`,
	} {
		t.Run(args, func(t *testing.T) {
			if _, err := tl.Execute(context.Background(), args); err == nil ||
				!errdefs.IsValidation(err) {
				t.Fatalf("Execute(%s) error = %v, want validation", args, err)
			}
		})
	}
}

func TestImportTool_Execute_PropagatesError(t *testing.T) {
	sentinel := errors.New("disk full")
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	impl := &errImport{err: sentinel}
	rt2, err := memory.New(memory.Spec{
		RuntimeID:    "prod",
		DefaultScope: memory.Scope{RuntimeID: "prod"},
	}, memory.Impls{
		Append: impl, Load: impl, Recall: impl,
		Import: impl, Compact: impl, Archive: impl,
	})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	tl, err := memtool.NewImportTool(rt2, validSettings())
	if err != nil {
		t.Fatalf("NewImportTool: %v", err)
	}
	_, err = tl.Execute(context.Background(), "")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	_ = rt // silence
}

// errImport is a tiny impl whose Import Execute returns a
// fixed error.
type errImport struct{ err error }

func (e *errImport) CompileImport(_ context.Context, _ memory.ImportRequest) memory.CompileResult {
	return allNativeImport(memory.OpImport, memory.FieldImportScope,
		memory.FieldImportDatasetID, memory.FieldImportSource,
		memory.FieldImportTags, memory.FieldImportChunkPolicy)
}
func (e *errImport) ExecuteImport(_ context.Context, _ memory.ImportRequest) (memory.ImportResponse, error) {
	return memory.ImportResponse{}, e.err
}
func (e *errImport) CompileAppend(_ context.Context, _ memory.AppendRequest) memory.CompileResult {
	return allNativeImport(memory.OpAppend, memory.FieldAppendScope,
		memory.FieldAppendConversationID, memory.FieldAppendIdempotencyKey,
		memory.FieldAppendRecords, memory.FieldAppendMetadata)
}
func (e *errImport) ExecuteAppend(_ context.Context, _ memory.AppendRequest) (memory.AppendResponse, error) {
	return memory.AppendResponse{}, nil
}
func (e *errImport) CompileLoad(_ context.Context, _ memory.LoadRequest) memory.CompileResult {
	return allNativeImport(memory.OpLoad, memory.FieldLoadScope,
		memory.FieldLoadConversationID, memory.FieldLoadCursor,
		memory.FieldLoadLimit, memory.FieldLoadReverse)
}
func (e *errImport) ExecuteLoad(_ context.Context, _ memory.LoadRequest) (memory.LoadResponse, error) {
	return memory.LoadResponse{}, nil
}
func (e *errImport) CompileRecall(_ context.Context, _ memory.RecallRequest) memory.CompileResult {
	return allNativeImport(memory.OpRecall, memory.FieldRecallScope,
		memory.FieldRecallConversationID, memory.FieldRecallQuery,
		memory.FieldRecallTopK, memory.FieldRecallFilters, memory.FieldRecallMinScore)
}
func (e *errImport) ExecuteRecall(_ context.Context, _ memory.RecallRequest) (memory.RecallResponse, error) {
	return memory.RecallResponse{}, nil
}
func (e *errImport) CompileCompact(_ context.Context, _ memory.CompactRequest) memory.CompileResult {
	return allNativeImport(memory.OpCompact, memory.FieldCompactScope,
		memory.FieldCompactOlderThan, memory.FieldCompactKeep)
}
func (e *errImport) ExecuteCompact(_ context.Context, _ memory.CompactRequest) (memory.CompactResponse, error) {
	return memory.CompactResponse{}, nil
}
func (e *errImport) CompileArchive(_ context.Context, _ memory.ArchiveRequest) memory.CompileResult {
	return allNativeImport(memory.OpArchive, memory.FieldArchiveScope,
		memory.FieldArchiveOlderThan, memory.FieldArchiveDestination)
}
func (e *errImport) ExecuteArchive(_ context.Context, _ memory.ArchiveRequest) (memory.ArchiveResponse, error) {
	return memory.ArchiveResponse{}, nil
}

func TestRegisterImportTool_RejectsNilRegistry(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	_, err := memtool.RegisterImportTool(nil, rt, validSettings())
	if err == nil {
		t.Fatal("expected nil-registry error")
	}
}

func TestRegisterImportTool_AddsToolToRegistry(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	reg := tool.NewRegistry()
	tl, err := memtool.RegisterImportTool(reg, rt, validSettings())
	if err != nil {
		t.Fatalf("RegisterImportTool: %v", err)
	}
	got, ok := reg.Get(memtool.ImportToolKind)
	if !ok {
		t.Fatalf("tool %q not in registry", memtool.ImportToolKind)
	}
	if got != tl {
		t.Errorf("registry returned different instance than constructor")
	}
	if got.Definition().Name != memtool.ImportToolKind {
		t.Errorf("Definition().Name = %q, want %q", got.Definition().Name, memtool.ImportToolKind)
	}
}

func TestRegisterImportTool_PropagatesValidationError(t *testing.T) {
	rt, _ := newRecordingRuntime(t, memory.Scope{RuntimeID: "prod"})
	reg := tool.NewRegistry()
	_, err := memtool.RegisterImportTool(reg, rt, memtool.ImportSettings{}) // empty
	if err == nil {
		t.Fatal("expected validation error")
	}
	if reg.Len() != 0 {
		t.Errorf("registry should be empty on failed register, got %d", reg.Len())
	}
}
