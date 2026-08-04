package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// allActiveScope is the scope every test uses as a starting
// point. RuntimeID is set so Validate passes.
func allActiveScope() Scope {
	return Scope{
		RuntimeID:      "prod",
		UserID:         "tenant-1",
		AgentID:        "researcher",
		ConversationID: "conv-42",
		DatasetID:      "kb",
	}
}

// oneMessage builds a minimal message.Message that passes
// inference's own Validate.
func oneMessage(text string) message.Message {
	return message.Message{
		Role: message.RoleUser,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: text},
		}},
	}
}

// oneRecord wraps oneMessage in a Record with a stable ID.
func oneRecord(id, text string) Record {
	return Record{ID: id, Message: oneMessage(text)}
}

func TestScopeValidate(t *testing.T) {
	t.Run("empty RuntimeID is rejected", func(t *testing.T) {
		err := Scope{}.Validate()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		memErr := AsError(err)
		if memErr == nil || memErr.Kind != KindScopeInvalid {
			t.Errorf("expected *Error with KindScopeInvalid, got: %T %v", err, err)
		}
		// The cause (reached via Unwrap) names the missing field.
		if !strings.Contains(memErr.cause.Error(), "RuntimeID") {
			t.Errorf("cause should mention RuntimeID, got: %v", memErr.cause)
		}
		// errdefs predicate also fires because the cause is
		// classified as Validation.
		if !errdefs.IsValidation(err) {
			t.Errorf("expected errdefs.IsValidation, got: %v", err)
		}
	})

	t.Run("non-empty RuntimeID passes", func(t *testing.T) {
		if err := (Scope{RuntimeID: "prod"}).Validate(); err != nil {
			t.Errorf("expected nil, got: %v", err)
		}
	})

	t.Run("empty UserID is the documented global scope", func(t *testing.T) {
		s := Scope{RuntimeID: "prod"}
		if err := s.Validate(); err != nil {
			t.Errorf("global scope must validate, got: %v", err)
		}
		// IsZero is the structural zero of the struct, not
		// the documented "global" override: a Scope with
		// only RuntimeID set is valid but not zero.
		if s.IsZero() {
			t.Errorf("Scope{RuntimeID: prod} is not the zero value")
		}
	})
}

func TestScopeHardPartitionKey(t *testing.T) {
	t.Run("combines RuntimeID and UserID with NUL separator", func(t *testing.T) {
		got := (Scope{RuntimeID: "prod", UserID: "u"}).HardPartitionKey()
		want := "prod\x00u"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("global scope has empty UserID component", func(t *testing.T) {
		got := (Scope{RuntimeID: "prod"}).HardPartitionKey()
		if got != "prod\x00" {
			t.Errorf("got %q, want %q", got, "prod\x00")
		}
	})

	t.Run("rejects NUL collision inputs", func(t *testing.T) {
		cases := []Scope{
			{RuntimeID: "a\x00b", UserID: "c"},
			{RuntimeID: "a", UserID: "b\x00c"},
		}
		for _, scope := range cases {
			if err := scope.Validate(); err == nil {
				t.Fatalf("Scope%+v: expected NUL validation error", scope)
			}
		}
	})
}

func TestNoopRuntimeAllOps(t *testing.T) {
	spec := Spec{RuntimeID: "prod"}
	rt, err := NewNoopRuntime(spec)
	if err != nil {
		t.Fatalf("NewNoopRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ctx := context.Background()
	scope := allActiveScope()

	t.Run("Append", func(t *testing.T) {
		req := AppendRequest{
			Scope:          scope,
			ConversationID: "conv-42",
			IdempotencyKey: "run-001",
			Records:        []Record{oneRecord("r-1", "hi")},
		}
		if _, err := rt.ExecuteAppend(ctx, req); err != nil {
			t.Errorf("Append: %v", err)
		}
	})

	t.Run("Load", func(t *testing.T) {
		req := LoadRequest{
			Scope:          scope,
			ConversationID: "conv-42",
			Limit:          10,
		}
		if _, err := rt.ExecuteLoad(ctx, req); err != nil {
			t.Errorf("Load: %v", err)
		}
	})

	t.Run("Recall", func(t *testing.T) {
		req := RecallRequest{
			Scope: scope,
			Query: "what is the cap?",
			TopK:  5,
		}
		if _, err := rt.ExecuteRecall(ctx, req); err != nil {
			t.Errorf("Recall: %v", err)
		}
	})

	t.Run("Import", func(t *testing.T) {
		req := ImportRequest{
			Scope:     scope,
			DatasetID: "kb",
			Source:    "file:///docs/spec.md",
		}
		if _, err := rt.ExecuteImport(ctx, req); err != nil {
			t.Errorf("Import: %v", err)
		}
	})

	t.Run("Compact", func(t *testing.T) {
		req := CompactRequest{
			Scope:     scope,
			OlderThan: time.Now().Add(-720 * time.Hour),
			Keep:      50,
		}
		if _, err := rt.ExecuteCompact(ctx, req); err != nil {
			t.Errorf("Compact: %v", err)
		}
	})

	t.Run("Archive", func(t *testing.T) {
		req := ArchiveRequest{
			Scope:       scope,
			OlderThan:   time.Now().Add(-4320 * time.Hour),
			Destination: "s3://bucket/cold",
		}
		if _, err := rt.ExecuteArchive(ctx, req); err != nil {
			t.Errorf("Archive: %v", err)
		}
	})
}

func TestRuntimeScopeMismatch(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{RuntimeID: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	req := AppendRequest{
		Scope:   Scope{RuntimeID: "different"},
		Records: []Record{oneRecord("r-1", "hi")},
	}
	_, err = rt.ExecuteAppend(context.Background(), req)
	if err == nil {
		t.Fatal("expected scope mismatch error")
	}
	if AsError(err) == nil || AsError(err).Kind != KindScopeInvalid {
		t.Errorf("expected KindScopeInvalid, got: %v", err)
	}
}

func TestRuntimeNotConfigured(t *testing.T) {
	// A runtime built with no Append impl should return
	// KindNotConfigured on Append, but other ops must still
	// fail the same way.
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{
		Recall: noopOps{}, // only Recall wired
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	scope := Scope{RuntimeID: "prod", UserID: "u"}
	_, err = rt.ExecuteAppend(context.Background(), AppendRequest{
		Scope:   scope,
		Records: []Record{oneRecord("r-1", "hi")},
	})
	if err == nil {
		t.Fatal("expected error for unwired Append")
	}
	if AsError(err) == nil || AsError(err).Kind != KindNotConfigured {
		t.Errorf("expected KindNotConfigured, got: %v", err)
	}

	// Recall must succeed.
	if _, err := rt.ExecuteRecall(context.Background(), RecallRequest{
		Scope: scope,
		Query: "hello",
		TopK:  3,
	}); err != nil {
		t.Errorf("wired Recall must succeed, got: %v", err)
	}
}

func TestCompileAppendRejectsMissingRecords(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{RuntimeID: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	res := rt.CompileAppend(context.Background(), AppendRequest{
		Scope: Scope{RuntimeID: "prod"},
	})
	if rej, ok := res.Rejected(); !ok || rej.Field != FieldAppendRecords {
		t.Errorf("expected rejection on FieldAppendRecords, got: %+v", res)
	}

	_, err = rt.ExecuteAppend(context.Background(), AppendRequest{
		Scope: Scope{RuntimeID: "prod"},
	})
	if err == nil {
		t.Fatal("expected error for empty Records")
	}
	if AsError(err) == nil || AsError(err).Kind != KindInvalidRequest {
		t.Errorf("expected KindInvalidRequest, got: %v", err)
	}
}

func TestCompileAppendRejectsDuplicateRecordIDs(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{RuntimeID: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	req := AppendRequest{
		Scope: Scope{RuntimeID: "prod"},
		Records: []Record{
			oneRecord("duplicate", "first"),
			oneRecord("duplicate", "second"),
		},
	}
	result := rt.CompileAppend(t.Context(), req)
	rejected, ok := result.Rejected()
	if !ok || rejected.Field != FieldAppendRecords ||
		rejected.Reason != ReasonInvalidValue {
		t.Fatalf("CompileAppend rejection = %+v, want append.records invalid_value", rejected)
	}
	if _, err := rt.ExecuteAppend(t.Context(), req); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("ExecuteAppend error = %v, want KindInvalidRequest", err)
	}
}

func TestAppendCompileAcceptsRuntimeAssignedIDsAndExecuteFillsThem(t *testing.T) {
	impl := &appendRecordSpy{}
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Append: impl})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	req := AppendRequest{
		Scope:   Scope{RuntimeID: "prod"},
		Records: []Record{{Message: oneRecord("", "hello").Message}},
	}
	if result := rt.CompileAppend(t.Context(), req); !result.AllNative() {
		t.Fatalf("CompileAppend = %+v, want all native", result)
	}
	if _, err := rt.ExecuteAppend(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(impl.compileRequests) != 2 {
		t.Fatalf("CompileAppend calls = %d, want 2", len(impl.compileRequests))
	}
	for index, compiled := range impl.compileRequests {
		if compiled.Records[0].ID != "" {
			t.Fatalf("compile request %d ID = %q, want runtime-managed empty ID", index, compiled.Records[0].ID)
		}
	}
	if len(impl.executeRequests) != 1 || impl.executeRequests[0].Records[0].ID == "" {
		t.Fatalf("execute requests = %+v, want generated record ID", impl.executeRequests)
	}
}

func TestCompileLoadRejectsZeroLimit(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{RuntimeID: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// No limit set on request, no default, no fallback ->
	// ledger should contain a Rejected decision for FieldLoadLimit.
	res := rt.CompileLoad(context.Background(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"},
	})
	if rej, ok := res.Rejected(); !ok || rej.Field != FieldLoadLimit {
		t.Errorf("expected rejection on FieldLoadLimit, got: %+v", res)
	}

	// Same call routed through ExecuteLoad must fail.
	_, err = rt.ExecuteLoad(context.Background(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"},
	})
	if err == nil {
		t.Fatal("expected error for unbounded Load")
	}
}

func TestCompileLoadFallsBackToDefault(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{
		RuntimeID:         "prod",
		DefaultLoadLimit:  25,
		FallbackLoadLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// DefaultLoadLimit fills in the gap.
	res := rt.CompileLoad(context.Background(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"},
	})
	if !res.AllNative() {
		t.Errorf("expected all Native, got: %+v", res)
	}
	if !res.HasField(FieldLoadLimit) {
		t.Errorf("expected FieldLoadLimit in ledger, got: %+v", res)
	}

	// ExecuteLoad must materialise the limit on the request
	// for the impl.
	called := false
	wire := loadSpy(func(_ context.Context, _ LoadRequest) (LoadResponse, error) {
		called = true
		return LoadResponse{}, nil
	})
	rt.impls.Load = wire
	if _, err := rt.ExecuteLoad(context.Background(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"},
	}); err != nil {
		t.Errorf("ExecuteLoad: %v", err)
	}
	if !called {
		t.Error("impl was not called")
	}
}

func TestRuntimeCloseIsIdempotent(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{RuntimeID: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Errorf("second Close must be no-op, got: %v", err)
	}
}

func TestRuntimeCloseRunsInReverseOrder(t *testing.T) {
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{})
	if err != nil {
		t.Fatal(err)
	}

	var order []int
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		i := i
		rt.RegisterClose(func() error {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return nil
		})
	}

	if err := rt.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	want := []int{2, 1, 0}
	if len(order) != len(want) {
		t.Fatalf("order length: got %d, want %d (%v)", len(order), len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d]: got %d, want %d", i, order[i], want[i])
		}
	}
}

func TestRuntimeCloseJoinsErrors(t *testing.T) {
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{})
	if err != nil {
		t.Fatal(err)
	}

	rt.RegisterClose(func() error { return errors.New("first") })
	rt.RegisterClose(func() error { return errors.New("second") })
	rt.RegisterClose(func() error { return nil })

	got := rt.Close()
	if got == nil {
		t.Fatal("expected joined error")
	}
	if !strings.Contains(got.Error(), "first") || !strings.Contains(got.Error(), "second") {
		t.Errorf("expected both errors in joined result, got: %v", got)
	}
}

func TestExecuteAppendClonesAndAssignsUniqueRecordIDs(t *testing.T) {
	var got AppendRequest
	impl := appendSpy(func(_ context.Context, req AppendRequest) (AppendResponse, error) {
		got = req
		return AppendResponse{}, nil
	})
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Append: impl})
	if err != nil {
		t.Fatal(err)
	}
	records := []Record{
		{Message: oneMessage("one")},
		{ID: "caller-id", Message: oneMessage("two")},
		{Message: oneMessage("three")},
	}
	if _, err := rt.ExecuteAppend(context.Background(), AppendRequest{
		Scope: Scope{RuntimeID: "prod"}, Records: records,
	}); err != nil {
		t.Fatal(err)
	}
	if records[0].ID != "" || records[2].ID != "" {
		t.Fatal("ExecuteAppend mutated caller-owned records")
	}
	seen := map[string]bool{}
	for i, record := range got.Records {
		if record.ID == "" {
			t.Fatalf("record %d has empty ID", i)
		}
		if seen[record.ID] {
			t.Fatalf("record %d reuses ID %q", i, record.ID)
		}
		seen[record.ID] = true
	}
}

func TestExecuteAppendRejectsDuplicateRecordIDs(t *testing.T) {
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Append: appendSpy(func(
		context.Context, AppendRequest,
	) (AppendResponse, error) {
		t.Fatal("impl must not be called")
		return AppendResponse{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.ExecuteAppend(context.Background(), AppendRequest{
		Scope: Scope{RuntimeID: "prod"},
		Records: []Record{
			{ID: "same", Message: oneMessage("one")},
			{ID: "same", Message: oneMessage("two")},
		},
	})
	if !IsKind(err, KindInvalidRequest) {
		t.Fatalf("error = %v, want KindInvalidRequest", err)
	}
}

func TestRuntimeCloseWaitsForExecuteAndRejectsNewWork(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	impl := loadSpy(func(_ context.Context, _ LoadRequest) (LoadResponse, error) {
		close(entered)
		<-release
		return LoadResponse{}, nil
	})
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Load: impl})
	if err != nil {
		t.Fatal(err)
	}
	closedStore := make(chan struct{})
	rt.RegisterClose(func() error {
		close(closedStore)
		return nil
	})
	executeDone := make(chan error, 1)
	go func() {
		_, err := rt.ExecuteLoad(context.Background(), LoadRequest{
			Scope: Scope{RuntimeID: "prod"}, Limit: 1,
		})
		executeDone <- err
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- rt.Close() }()
	select {
	case <-closedStore:
		t.Fatal("closer ran while Execute was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	_, err = rt.ExecuteLoad(context.Background(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"}, Limit: 1,
	})
	if !IsKind(err, KindNotConfigured) {
		t.Fatalf("post-close Execute error = %v, want KindNotConfigured", err)
	}
	close(release)
	if err := <-executeDone; err != nil {
		t.Fatalf("in-flight Execute: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-closedStore
}

func TestRuntimeCloseWaitsForCompileAndCompileAfterCloseDoesNotCallImpl(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	impl := compileLoadOp{compile: func(req LoadRequest) CompileResult {
		calls.Add(1)
		close(entered)
		<-release
		return nativeLoadCompile(req)
	}}
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Load: impl})
	if err != nil {
		t.Fatal(err)
	}
	closedStore := make(chan struct{})
	rt.RegisterClose(func() error {
		close(closedStore)
		return nil
	})
	compileDone := make(chan CompileResult, 1)
	go func() {
		compileDone <- rt.CompileLoad(t.Context(), LoadRequest{
			Scope: Scope{RuntimeID: "prod"}, Limit: 1,
		})
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- rt.Close() }()
	select {
	case <-closedStore:
		t.Fatal("closer ran while Compile was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if result := <-compileDone; !result.AllNative() {
		t.Fatalf("in-flight CompileLoad = %+v", result)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}

	result := rt.CompileLoad(t.Context(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"}, Limit: 1,
	})
	if calls.Load() != 1 {
		t.Fatalf("implementation compile calls = %d, want 1", calls.Load())
	}
	if len(result.Decisions) != len(loadActiveFields(LoadRequest{})) {
		t.Fatalf("post-close ledger = %+v", result)
	}
	for _, decision := range result.Decisions {
		if decision.Disposition != DispositionRejected ||
			decision.Reason != ReasonNotConfigured {
			t.Fatalf("post-close decision = %+v, want not configured", decision)
		}
	}
}

func TestRuntimeCompileAfterCloseReturnsCompleteNotConfiguredLedgers(t *testing.T) {
	impl := &countingOps{noopOps: noopOps{}}
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{
		Append: impl, Load: impl, Recall: impl,
		Import: impl, Compact: impl, Archive: impl,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "prod"}
	cutoff := time.Now().Add(-time.Hour)
	results := []struct {
		result CompileResult
		fields []FieldID
	}{
		{rt.CompileAppend(t.Context(), AppendRequest{
			Scope: scope, Records: []Record{oneRecord("a", "x")},
		}), appendActiveFields(AppendRequest{})},
		{rt.CompileLoad(t.Context(), LoadRequest{
			Scope: scope, Limit: 1,
		}), loadActiveFields(LoadRequest{})},
		{rt.CompileRecall(t.Context(), RecallRequest{
			Scope: scope, Query: "q", TopK: 1,
		}), recallActiveFields(RecallRequest{})},
		{rt.CompileImport(t.Context(), ImportRequest{
			Scope: scope, Source: "memory://x",
		}), importActiveFields(ImportRequest{})},
		{rt.CompileCompact(t.Context(), CompactRequest{
			Scope: scope, OlderThan: cutoff,
		}), compactActiveFields(CompactRequest{})},
		{rt.CompileArchive(t.Context(), ArchiveRequest{
			Scope: scope, OlderThan: cutoff, Destination: "memory://archive",
		}), archiveActiveFields(ArchiveRequest{})},
	}
	if impl.calls != 0 {
		t.Fatalf("implementation calls after Close = %d, want 0", impl.calls)
	}
	for _, item := range results {
		if len(item.result.Decisions) != len(item.fields) {
			t.Fatalf("%s decisions = %d, want %d",
				item.result.Op, len(item.result.Decisions), len(item.fields))
		}
		for _, decision := range item.result.Decisions {
			if decision.Disposition != DispositionRejected ||
				decision.Reason != ReasonNotConfigured {
				t.Fatalf("%s post-close decision = %+v", item.result.Op, decision)
			}
		}
	}
}

func TestSpecValidateRejectsInvalidDefaultScope(t *testing.T) {
	_, err := New(Spec{
		RuntimeID: "prod",
		DefaultScope: Scope{
			RuntimeID: "prod",
			UserID:    "bad\x00tenant",
		},
	}, Impls{})
	memErr := AsError(err)
	if memErr == nil || memErr.Kind != KindScopeInvalid ||
		memErr.cause == nil || !strings.Contains(memErr.cause.Error(), "NUL") {
		t.Fatalf("New error = %v, want invalid DefaultScope", err)
	}
}

func TestSpecValidateRejectsNULRuntimeID(t *testing.T) {
	_, err := New(Spec{RuntimeID: "bad\x00runtime"}, Impls{})
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("New error = %v, want RuntimeID NUL validation error", err)
	}
}

func TestRuntimeMaintenanceCompileRejectsInvalidPolicy(t *testing.T) {
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{
		Compact: noopOps{},
		Archive: noopOps{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	scope := Scope{RuntimeID: "prod"}
	tests := []struct {
		name  string
		field FieldID
		run   func() CompileResult
	}{
		{
			name:  "compact zero older than",
			field: FieldCompactOlderThan,
			run: func() CompileResult {
				return rt.CompileCompact(t.Context(), CompactRequest{Scope: scope})
			},
		},
		{
			name:  "compact negative keep",
			field: FieldCompactKeep,
			run: func() CompileResult {
				return rt.CompileCompact(t.Context(), CompactRequest{
					Scope: scope, OlderThan: time.Now(), Keep: -1,
				})
			},
		},
		{
			name:  "archive zero older than",
			field: FieldArchiveOlderThan,
			run: func() CompileResult {
				return rt.CompileArchive(t.Context(), ArchiveRequest{
					Scope: scope, Destination: "memory://archive",
				})
			},
		},
		{
			name:  "archive empty destination",
			field: FieldArchiveDestination,
			run: func() CompileResult {
				return rt.CompileArchive(t.Context(), ArchiveRequest{
					Scope: scope, OlderThan: time.Now(), Destination: " ",
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rejected, ok := test.run().Rejected()
			if !ok || rejected.Field != test.field ||
				rejected.Reason != ReasonInvalidValue {
				t.Fatalf("rejection = %+v, want %s invalid", rejected, test.field)
			}
		})
	}
}

func TestNoopRuntimeRejectsInvalidMaintenancePolicy(t *testing.T) {
	rt, err := NewNoopRuntime(Spec{RuntimeID: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	scope := Scope{RuntimeID: "prod"}
	if _, err := rt.ExecuteCompact(t.Context(), CompactRequest{
		Scope: scope, OlderThan: time.Now(), Keep: -1,
	}); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("ExecuteCompact error = %v, want KindInvalidRequest", err)
	}
	if _, err := rt.ExecuteArchive(t.Context(), ArchiveRequest{
		Scope: scope, OlderThan: time.Now(),
	}); !IsKind(err, KindInvalidRequest) {
		t.Fatalf("ExecuteArchive error = %v, want KindInvalidRequest", err)
	}
}

func TestRuntimeRegisterCloseAfterCloseRunsImmediately(t *testing.T) {
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	rt.RegisterClose(func() error {
		called = true
		return nil
	})
	if !called {
		t.Fatal("late closer was not run")
	}
}

func TestErrorAsUnwraps(t *testing.T) {
	cause := errors.New("disk full")
	wrapped := wrapErr(KindProviderFailure, OpImport, "", cause)
	if AsError(wrapped) == nil {
		t.Fatal("expected *Error, got nil")
	}
	if !errors.Is(wrapped, cause) {
		t.Errorf("expected errors.Is to find the cause")
	}
}

func TestErrorClassifiesUnderErrdefs(t *testing.T) {
	// Each ErrorKind must map to the right errdefs predicate
	// so callers can use the sdk-wide helpers in addition to
	// the memory-specific Kind / IsKind.
	cases := []struct {
		name    string
		kind    ErrorKind
		wantErr func(error) bool
	}{
		{"InvalidRequest", KindInvalidRequest, errdefs.IsValidation},
		{"ScopeInvalid", KindScopeInvalid, errdefs.IsValidation},
		{"UnsupportedFeature", KindUnsupportedFeature, errdefs.IsValidation},
		{"InvalidExtension", KindInvalidExtension, errdefs.IsValidation},
		{"NotConfigured", KindNotConfigured, errdefs.IsNotAvailable},
		{"PolicyDenied", KindPolicyDenied, errdefs.IsPolicyDenied},
		{"OperationInterrupted", KindOperationInterrupted, errdefs.IsInterrupted},
		{"ProviderFailure", KindProviderFailure, errdefs.IsNotAvailable},
		{"Internal", KindInternal, errdefs.IsInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := newError(tc.kind, OpAppend, "", errors.New("x")).Error()
			// newError returns *Error; call .Error() to get a value the
			// predicate accepts.
			var e error = newError(tc.kind, OpAppend, "", errors.New("x"))
			if !tc.wantErr(e) {
				t.Errorf("errdefs predicate for %s did not match: %v", tc.name, e)
			}
			_ = err
		})
	}
}

func TestErrorProviderFailurePreservesCauseClassification(t *testing.T) {
	// If the impl returns a pre-classified error (e.g. a
	// timeout), the wrapper must keep that classification so
	// callers can still tell it was a timeout — the cause's
	// errdefs kind wins, KindProviderFailure is just the
	// memory-side label.
	timeout := errdefs.Timeout(errors.New("transport timeout"))
	wrapped := wrapErr(KindProviderFailure, OpImport, "", timeout)
	if !errdefs.IsTimeout(wrapped) {
		t.Errorf("expected errdefs.IsTimeout on wrapped error, got: %v", wrapped)
	}
	// Plain ProviderFailure without a classified cause falls
	// back to NotAvailable, since "the provider failed" is the
	// closest match in the errdefs taxonomy.
	plain := wrapErr(KindProviderFailure, OpImport, "", errors.New("disk full"))
	if !errdefs.IsNotAvailable(plain) {
		t.Errorf("expected errdefs.IsNotAvailable on plain provider failure, got: %v", plain)
	}
}

func TestErrorIsKind(t *testing.T) {
	if !IsKind(newError(KindScopeInvalid, "", "", nil), KindScopeInvalid) {
		t.Error("IsKind should match the same kind")
	}
	if IsKind(newError(KindScopeInvalid, "", "", nil), KindInternal) {
		t.Error("IsKind should not match a different kind")
	}
	if IsKind(errors.New("plain"), KindScopeInvalid) {
		t.Error("IsKind on a non-memory error must return false")
	}
}

func TestRuntimeCompileReflectsImplRejectionAndNilImpl(t *testing.T) {
	rejecting := compileLoadOp{
		compile: func(req LoadRequest) CompileResult {
			result := nativeLoadCompile(req)
			for i := range result.Decisions {
				if result.Decisions[i].Field == FieldLoadReverse {
					result.Decisions[i] = rejectedDecision(
						FieldLoadReverse, ReasonUnsupportedFeature, "reverse is unsupported")
				}
			}
			return result
		},
		execute: func(LoadRequest) (LoadResponse, error) {
			t.Fatal("ExecuteLoad must not run after compile rejection")
			return LoadResponse{}, nil
		},
	}
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Load: rejecting})
	if err != nil {
		t.Fatal(err)
	}
	req := LoadRequest{Scope: Scope{RuntimeID: "prod"}, Limit: 1, Reverse: true}
	result := rt.CompileLoad(t.Context(), req)
	if rejected, ok := result.Rejected(); !ok ||
		rejected.Field != FieldLoadReverse ||
		rejected.Reason != ReasonUnsupportedFeature {
		t.Fatalf("CompileLoad rejection = %+v", result)
	}
	if _, err := rt.ExecuteLoad(t.Context(), req); !IsKind(err, KindUnsupportedFeature) {
		t.Fatalf("ExecuteLoad error = %v, want KindUnsupportedFeature", err)
	}

	nilRuntime, err := New(Spec{RuntimeID: "prod"}, Impls{})
	if err != nil {
		t.Fatal(err)
	}
	nilResult := nilRuntime.CompileLoad(t.Context(), req)
	if len(nilResult.Decisions) != len(loadActiveFields(req)) {
		t.Fatalf("nil impl decisions = %d, want %d",
			len(nilResult.Decisions), len(loadActiveFields(req)))
	}
	for _, decision := range nilResult.Decisions {
		if decision.Disposition != DispositionRejected ||
			decision.Reason != ReasonNotConfigured ||
			decision.Message == "" {
			t.Errorf("nil impl decision is incomplete: %+v", decision)
		}
	}
}

func TestRuntimeCompileMaterializesDefaultsForImplAndExecuteCompilesOnce(t *testing.T) {
	var loadCompileCalls, recallCompileCalls int
	var loadLimit, recallTopK int
	impl := &defaultCompileSpy{
		loadCompile: func(req LoadRequest) CompileResult {
			loadCompileCalls++
			loadLimit = req.Limit
			return nativeLoadCompile(req)
		},
		recallCompile: func(req RecallRequest) CompileResult {
			recallCompileCalls++
			recallTopK = req.TopK
			return nativeRecallCompile(req)
		},
	}
	rt, err := New(Spec{
		RuntimeID:        "prod",
		DefaultLoadLimit: 17,
		DefaultTopK:      9,
	}, Impls{Load: impl, Recall: impl})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "prod"}
	if result := rt.CompileLoad(t.Context(), LoadRequest{Scope: scope}); !result.AllNative() {
		t.Fatalf("CompileLoad = %+v", result)
	}
	if loadLimit != 17 {
		t.Fatalf("CompileLoad impl Limit = %d, want 17", loadLimit)
	}
	loadCompileCalls = 0
	if _, err := rt.ExecuteLoad(t.Context(), LoadRequest{Scope: scope}); err != nil {
		t.Fatal(err)
	}
	if loadCompileCalls != 1 || loadLimit != 17 {
		t.Fatalf("ExecuteLoad compile calls/Limit = %d/%d, want 1/17",
			loadCompileCalls, loadLimit)
	}

	if result := rt.CompileRecall(t.Context(), RecallRequest{
		Scope: scope, Query: "q",
	}); !result.AllNative() {
		t.Fatalf("CompileRecall = %+v", result)
	}
	if recallTopK != 9 {
		t.Fatalf("CompileRecall impl TopK = %d, want 9", recallTopK)
	}
	recallCompileCalls = 0
	if _, err := rt.ExecuteRecall(t.Context(), RecallRequest{
		Scope: scope, Query: "q",
	}); err != nil {
		t.Fatal(err)
	}
	if recallCompileCalls != 1 || recallTopK != 9 {
		t.Fatalf("ExecuteRecall compile calls/TopK = %d/%d, want 1/9",
			recallCompileCalls, recallTopK)
	}
}

func TestRuntimeScopeDefaulting(t *testing.T) {
	impl := &scopeSpyOps{noopOps: noopOps{}}
	defaultScope := Scope{RuntimeID: "prod", UserID: "tenant-default"}
	rt, err := New(Spec{
		RuntimeID:    "prod",
		DefaultScope: defaultScope,
	}, Impls{
		Append: impl, Load: impl, Recall: impl,
		Import: impl, Compact: impl, Archive: impl,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	cutoff := time.Now().Add(-time.Hour)
	compileCalls := []func(){
		func() { rt.CompileAppend(ctx, AppendRequest{Records: []Record{oneRecord("a", "x")}}) },
		func() { rt.CompileLoad(ctx, LoadRequest{Limit: 1}) },
		func() { rt.CompileRecall(ctx, RecallRequest{Query: "q", TopK: 1}) },
		func() { rt.CompileImport(ctx, ImportRequest{Source: "memory://x"}) },
		func() { rt.CompileCompact(ctx, CompactRequest{OlderThan: cutoff}) },
		func() {
			rt.CompileArchive(ctx, ArchiveRequest{
				OlderThan: cutoff, Destination: "memory://archive",
			})
		},
	}
	for _, compile := range compileCalls {
		compile()
	}
	if len(impl.scopes) != 6 {
		t.Fatalf("compile scopes = %d, want 6", len(impl.scopes))
	}
	for i, scope := range impl.scopes {
		if scope != defaultScope {
			t.Errorf("compile scope %d = %+v, want %+v", i, scope, defaultScope)
		}
	}
	impl.scopes = nil
	executeCalls := []func() error{
		func() error {
			_, err := rt.ExecuteAppend(ctx, AppendRequest{Records: []Record{oneRecord("a", "x")}})
			return err
		},
		func() error { _, err := rt.ExecuteLoad(ctx, LoadRequest{Limit: 1}); return err },
		func() error {
			_, err := rt.ExecuteRecall(ctx, RecallRequest{Query: "q", TopK: 1})
			return err
		},
		func() error {
			_, err := rt.ExecuteImport(ctx, ImportRequest{Source: "memory://x"})
			return err
		},
		func() error {
			_, err := rt.ExecuteCompact(ctx, CompactRequest{OlderThan: cutoff})
			return err
		},
		func() error {
			_, err := rt.ExecuteArchive(ctx, ArchiveRequest{
				OlderThan: cutoff, Destination: "memory://archive",
			})
			return err
		},
	}
	for i, execute := range executeCalls {
		if err := execute(); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	if len(impl.scopes) != 6 {
		t.Fatalf("execute compile scopes = %d, want 6", len(impl.scopes))
	}
	for i, scope := range impl.scopes {
		if scope != defaultScope {
			t.Errorf("execute scope %d = %+v, want %+v", i, scope, defaultScope)
		}
	}

	impl.scopes = nil
	rt.CompileLoad(ctx, LoadRequest{
		Scope: Scope{RuntimeID: "prod"},
		Limit: 1,
	})
	if got := impl.scopes[0]; got.UserID != "" {
		t.Fatalf("explicit global scope UserID = %q, want empty", got.UserID)
	}
}

func TestRuntimeRejectsInvalidLedgerSemantics(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
	}{
		{
			name: "unknown disposition",
			decision: Decision{
				Field: FieldLoadLimit, Disposition: Disposition("forwarded"),
			},
		},
		{
			name: "rejected without reason",
			decision: Decision{
				Field: FieldLoadLimit, Disposition: DispositionRejected,
			},
		},
		{
			name: "native with reason",
			decision: Decision{
				Field: FieldLoadLimit, Disposition: DispositionNative,
				Reason: ReasonInvalidValue,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			impl := compileLoadOp{
				compile: func(req LoadRequest) CompileResult {
					result := nativeLoadCompile(req)
					for i := range result.Decisions {
						if result.Decisions[i].Field == FieldLoadLimit {
							result.Decisions[i] = test.decision
						}
					}
					return result
				},
			}
			rt, err := New(Spec{RuntimeID: "prod"}, Impls{Load: impl})
			if err != nil {
				t.Fatal(err)
			}
			_, err = rt.ExecuteLoad(t.Context(), LoadRequest{
				Scope: Scope{RuntimeID: "prod"}, Limit: 1,
			})
			if !IsKind(err, KindInternal) {
				t.Fatalf("error = %v, want KindInternal", err)
			}
		})
	}
}

func TestRuntimeCancellationSkipsAllImplementations(t *testing.T) {
	impl := &countingOps{noopOps: noopOps{}}
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{
		Append: impl, Load: impl, Recall: impl,
		Import: impl, Compact: impl, Archive: impl,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scope := Scope{RuntimeID: "prod"}
	calls := []func() error{
		func() error {
			_, err := rt.ExecuteAppend(ctx, AppendRequest{
				Scope: scope, Records: []Record{oneRecord("a", "x")},
			})
			return err
		},
		func() error {
			_, err := rt.ExecuteLoad(ctx, LoadRequest{Scope: scope, Limit: 1})
			return err
		},
		func() error {
			_, err := rt.ExecuteRecall(ctx, RecallRequest{Scope: scope, Query: "q", TopK: 1})
			return err
		},
		func() error {
			_, err := rt.ExecuteImport(ctx, ImportRequest{Scope: scope, Source: "memory://x"})
			return err
		},
		func() error { _, err := rt.ExecuteCompact(ctx, CompactRequest{Scope: scope}); return err },
		func() error { _, err := rt.ExecuteArchive(ctx, ArchiveRequest{Scope: scope}); return err },
	}
	for i, call := range calls {
		if err := call(); !IsKind(err, KindOperationInterrupted) {
			t.Errorf("call %d error = %v, want KindOperationInterrupted", i, err)
		}
	}
	if impl.calls != 0 {
		t.Fatalf("implementation calls = %d, want 0", impl.calls)
	}
}

func TestRuntimeMapsImplContextErrorToInterrupted(t *testing.T) {
	impl := compileLoadOp{
		execute: func(LoadRequest) (LoadResponse, error) {
			return LoadResponse{}, context.DeadlineExceeded
		},
	}
	rt, err := New(Spec{RuntimeID: "prod"}, Impls{Load: impl})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.ExecuteLoad(t.Context(), LoadRequest{
		Scope: Scope{RuntimeID: "prod"}, Limit: 1,
	})
	if !IsKind(err, KindOperationInterrupted) {
		t.Fatalf("error = %v, want KindOperationInterrupted", err)
	}
}

// --- test helpers -------------------------------------------------

// loadSpy adapts a plain function to the LoadOp interface so a
// test can assert it was invoked. Its CompileLoad mirrors what
// a faithful noop would do: emit Native for every active field
// the runtime is going to enforce against.
type loadSpy func(context.Context, LoadRequest) (LoadResponse, error)

func (f loadSpy) CompileLoad(_ context.Context, req LoadRequest) CompileResult {
	fields := loadActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, field := range fields {
		decisions[i] = nativeDecision(field)
	}
	return CompileResult{Op: OpLoad, Decisions: decisions}
}
func (f loadSpy) ExecuteLoad(ctx context.Context, req LoadRequest) (LoadResponse, error) {
	return f(ctx, req)
}

type appendSpy func(context.Context, AppendRequest) (AppendResponse, error)

func (f appendSpy) CompileAppend(_ context.Context, req AppendRequest) CompileResult {
	fields := appendActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, field := range fields {
		decisions[i] = nativeDecision(field)
	}
	return CompileResult{Op: OpAppend, Decisions: decisions}
}

func (f appendSpy) ExecuteAppend(ctx context.Context, req AppendRequest) (AppendResponse, error) {
	return f(ctx, req)
}

type appendRecordSpy struct {
	compileRequests []AppendRequest
	executeRequests []AppendRequest
}

func (s *appendRecordSpy) CompileAppend(_ context.Context, req AppendRequest) CompileResult {
	s.compileRequests = append(s.compileRequests, req)
	return noopOps{}.CompileAppend(context.Background(), req)
}

func (s *appendRecordSpy) ExecuteAppend(_ context.Context, req AppendRequest) (AppendResponse, error) {
	s.executeRequests = append(s.executeRequests, req)
	return AppendResponse{}, nil
}

type compileLoadOp struct {
	compile func(LoadRequest) CompileResult
	execute func(LoadRequest) (LoadResponse, error)
}

func (op compileLoadOp) CompileLoad(_ context.Context, req LoadRequest) CompileResult {
	if op.compile != nil {
		return op.compile(req)
	}
	return nativeLoadCompile(req)
}

func (op compileLoadOp) ExecuteLoad(_ context.Context, req LoadRequest) (LoadResponse, error) {
	if op.execute != nil {
		return op.execute(req)
	}
	return LoadResponse{}, nil
}

func nativeLoadCompile(req LoadRequest) CompileResult {
	fields := loadActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, field := range fields {
		decisions[i] = nativeDecision(field)
	}
	return CompileResult{Op: OpLoad, Decisions: decisions}
}

func nativeRecallCompile(req RecallRequest) CompileResult {
	fields := recallActiveFields(req)
	decisions := make([]Decision, len(fields))
	for i, field := range fields {
		decisions[i] = nativeDecision(field)
	}
	return CompileResult{Op: OpRecall, Decisions: decisions}
}

type defaultCompileSpy struct {
	noopOps
	loadCompile   func(LoadRequest) CompileResult
	recallCompile func(RecallRequest) CompileResult
}

func (s *defaultCompileSpy) CompileLoad(_ context.Context, req LoadRequest) CompileResult {
	return s.loadCompile(req)
}

func (s *defaultCompileSpy) CompileRecall(_ context.Context, req RecallRequest) CompileResult {
	return s.recallCompile(req)
}

type scopeSpyOps struct {
	noopOps
	scopes []Scope
}

func (s *scopeSpyOps) record(scope Scope) {
	s.scopes = append(s.scopes, scope)
}

func (s *scopeSpyOps) CompileAppend(ctx context.Context, req AppendRequest) CompileResult {
	s.record(req.Scope)
	return s.noopOps.CompileAppend(ctx, req)
}

func (s *scopeSpyOps) CompileLoad(ctx context.Context, req LoadRequest) CompileResult {
	s.record(req.Scope)
	return s.noopOps.CompileLoad(ctx, req)
}

func (s *scopeSpyOps) CompileRecall(ctx context.Context, req RecallRequest) CompileResult {
	s.record(req.Scope)
	return s.noopOps.CompileRecall(ctx, req)
}

func (s *scopeSpyOps) CompileImport(ctx context.Context, req ImportRequest) CompileResult {
	s.record(req.Scope)
	return s.noopOps.CompileImport(ctx, req)
}

func (s *scopeSpyOps) CompileCompact(ctx context.Context, req CompactRequest) CompileResult {
	s.record(req.Scope)
	return s.noopOps.CompileCompact(ctx, req)
}

func (s *scopeSpyOps) CompileArchive(ctx context.Context, req ArchiveRequest) CompileResult {
	s.record(req.Scope)
	return s.noopOps.CompileArchive(ctx, req)
}

type countingOps struct {
	noopOps
	calls int
}

func (s *countingOps) count() { s.calls++ }

func (s *countingOps) CompileAppend(ctx context.Context, req AppendRequest) CompileResult {
	s.count()
	return s.noopOps.CompileAppend(ctx, req)
}

func (s *countingOps) ExecuteAppend(ctx context.Context, req AppendRequest) (AppendResponse, error) {
	s.count()
	return s.noopOps.ExecuteAppend(ctx, req)
}

func (s *countingOps) CompileLoad(ctx context.Context, req LoadRequest) CompileResult {
	s.count()
	return s.noopOps.CompileLoad(ctx, req)
}

func (s *countingOps) ExecuteLoad(ctx context.Context, req LoadRequest) (LoadResponse, error) {
	s.count()
	return s.noopOps.ExecuteLoad(ctx, req)
}

func (s *countingOps) CompileRecall(ctx context.Context, req RecallRequest) CompileResult {
	s.count()
	return s.noopOps.CompileRecall(ctx, req)
}

func (s *countingOps) ExecuteRecall(ctx context.Context, req RecallRequest) (RecallResponse, error) {
	s.count()
	return s.noopOps.ExecuteRecall(ctx, req)
}

func (s *countingOps) CompileImport(ctx context.Context, req ImportRequest) CompileResult {
	s.count()
	return s.noopOps.CompileImport(ctx, req)
}

func (s *countingOps) ExecuteImport(ctx context.Context, req ImportRequest) (ImportResponse, error) {
	s.count()
	return s.noopOps.ExecuteImport(ctx, req)
}

func (s *countingOps) CompileCompact(ctx context.Context, req CompactRequest) CompileResult {
	s.count()
	return s.noopOps.CompileCompact(ctx, req)
}

func (s *countingOps) ExecuteCompact(ctx context.Context, req CompactRequest) (CompactResponse, error) {
	s.count()
	return s.noopOps.ExecuteCompact(ctx, req)
}

func (s *countingOps) CompileArchive(ctx context.Context, req ArchiveRequest) CompileResult {
	s.count()
	return s.noopOps.CompileArchive(ctx, req)
}

func (s *countingOps) ExecuteArchive(ctx context.Context, req ArchiveRequest) (ArchiveResponse, error) {
	s.count()
	return s.noopOps.ExecuteArchive(ctx, req)
}
