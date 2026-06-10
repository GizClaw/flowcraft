package fact

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestLedgerWorkspaceStoreNilWorkspaceReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(nil)

	if _, err := store.Put(ctx, validFact("fact-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.Get(ctx, "fact-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil workspace error = %v, want validation", err)
	}
	if _, err := store.List(ctx, ListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil workspace error = %v, want validation", err)
	}
	if err := store.Delete(ctx, "fact-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteSubject(ctx, "user:123"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteSubject nil workspace error = %v, want validation", err)
	}
}

func TestLedgerWorkspaceStorePutNormalizesStatusAndGetListDeepClone(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	fact := validFact("fact-1")
	fact.Status = ""

	put, err := store.Put(ctx, fact)
	if err != nil {
		t.Fatal(err)
	}
	if put.Status != FactActive {
		t.Fatalf("Put status = %q, want active", put.Status)
	}

	fact.SourceRefs[0].Message.MessageID = "mutated-input"
	fact.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-input"
	fact.Signature.DiagnosticSignatures["reconciler"] = "mutated-input"
	fact.Metadata["k"] = "mutated-input"
	setNestedMetadata(fact.Metadata, "mutated-input")
	*fact.ValidFrom = fact.ValidFrom.AddDate(0, 0, 1)

	put.SourceRefs[0].Message.MessageID = "mutated-put"
	put.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-put"
	put.Signature.DiagnosticSignatures["reconciler"] = "mutated-put"
	put.Metadata["k"] = "mutated-put"
	setNestedMetadata(put.Metadata, "mutated-put")
	*put.ValidFrom = put.ValidFrom.AddDate(0, 0, 1)

	want := validFact("fact-1")
	got, ok, err := store.Get(ctx, "fact-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	assertFactEqual(t, got, want)

	got.SourceRefs[0].Message.MessageID = "mutated-get"
	got.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-get"
	got.Signature.DiagnosticSignatures["reconciler"] = "mutated-get"
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	*got.ValidFrom = got.ValidFrom.AddDate(0, 0, 1)

	listed, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d facts, want 1", len(listed))
	}
	listed[0].SourceRefs[0].Message.MessageID = "mutated-list"
	listed[0].Signature.UpstreamViewRefs[0].OutputSignature = "mutated-list"
	listed[0].Signature.DiagnosticSignatures["reconciler"] = "mutated-list"
	listed[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listed[0].Metadata, "mutated-list")
	*listed[0].ValidFrom = listed[0].ValidFrom.AddDate(0, 0, 1)

	again, ok, err := store.Get(ctx, "fact-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get after List mutation ok = false, want true")
	}
	assertFactEqual(t, again, want)
}

func TestLedgerWorkspaceStoreListOrderAfterIDLimitAndFilters(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())

	facts := map[FactID]Fact{
		"bravo":   validFact("bravo"),
		"alpha":   validFact("alpha"),
		"delta":   validFact("delta"),
		"charlie": validFact("charlie"),
	}
	bravo := facts["bravo"]
	bravo.Subject = "user:456"
	facts["bravo"] = bravo
	charlie := facts["charlie"]
	charlie.Predicate = "dislikes"
	facts["charlie"] = charlie
	delta := facts["delta"]
	delta.Status = FactSuperseded
	facts["delta"] = delta

	for _, id := range []FactID{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.Put(ctx, facts[id]); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, all, []FactID{"alpha", "bravo", "charlie", "delta"})

	afterLimited, err := store.List(ctx, ListOptions{AfterID: "alpha", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, afterLimited, []FactID{"bravo", "charlie"})

	subjectFiltered, err := store.List(ctx, ListOptions{Subject: "user:123"})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, subjectFiltered, []FactID{"alpha", "charlie", "delta"})

	predicateFiltered, err := store.List(ctx, ListOptions{Predicate: "likes"})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, predicateFiltered, []FactID{"alpha", "bravo", "delta"})

	status := FactSuperseded
	statusFiltered, err := store.List(ctx, ListOptions{Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, statusFiltered, []FactID{"delta"})

	active := FactActive
	combined, err := store.List(ctx, ListOptions{
		Limit:     2,
		Subject:   "user:123",
		Predicate: "likes",
		Status:    &active,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, combined, []FactID{"alpha"})
}

func TestLedgerWorkspaceStoreDeleteOneFact(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())

	if _, err := store.Put(ctx, validFact("fact-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, validFact("fact-2")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "fact-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "fact-1"); err != nil || ok {
		t.Fatalf("Get deleted fact ok = %v err %v, want false nil", ok, err)
	}
	if err := store.Delete(ctx, "fact-1"); err != nil {
		t.Fatalf("second Delete error = %v, want nil", err)
	}
	if got, ok, err := store.Get(ctx, "fact-2"); err != nil || !ok || got.ID != "fact-2" {
		t.Fatalf("Get kept fact = %+v ok %v err %v, want fact-2 true nil", got, ok, err)
	}
}

func TestLedgerWorkspaceStoreDeleteSubjectOnlyDeletesMatchingSubject(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())

	factOne := validFact("fact-1")
	factTwo := validFact("fact-2")
	factTwo.Subject = "user:456"
	factThree := validFact("fact-3")

	for _, fact := range []Fact{factOne, factTwo, factThree} {
		if _, err := store.Put(ctx, fact); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteSubject(ctx, "user:123"); err != nil {
		t.Fatal(err)
	}

	listed, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertFactIDs(t, listed, []FactID{"fact-2"})
	if listed[0].Subject != "user:456" {
		t.Fatalf("remaining subject = %q, want user:456", listed[0].Subject)
	}

	if err := store.DeleteSubject(ctx, "user:123"); err != nil {
		t.Fatalf("second DeleteSubject error = %v, want nil", err)
	}
}

func TestLedgerWorkspaceStorePathSegmentPrefixDefaultCustomAndExplicitEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("default", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewLedgerWorkspaceStore(ws)
		fact := validFact("fact/with/slash")
		if _, err := store.Put(ctx, fact); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(fact.ID))
		assertSafeFactSegment(t, store, segment, string(fact.ID), "fact_")
		assertFactPathExists(t, ctx, ws, "facts/"+segment+".json")
		assertFactPathMissing(t, ctx, ws, "facts/"+string(fact.ID)+".json")
	})

	t.Run("custom", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewLedgerWorkspaceStore(ws, WithFactPathSegmentPrefix("custom_"))
		fact := validFact("../fact.json")
		if _, err := store.Put(ctx, fact); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(fact.ID))
		assertSafeFactSegment(t, store, segment, string(fact.ID), "custom_")
		assertFactPathExists(t, ctx, ws, "facts/"+segment+".json")
		assertFactPathMissing(t, ctx, ws, "facts/"+string(fact.ID)+".json")
	})

	t.Run("explicit empty", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewLedgerWorkspaceStore(ws, WithFactPathSegmentPrefix(""))
		fact := validFact("fact/with/slash")
		if _, err := store.Put(ctx, fact); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(fact.ID))
		if strings.HasPrefix(segment, defaultFactPathSegmentPrefix) {
			t.Fatalf("explicit empty prefix segment = %q, should not use default prefix", segment)
		}
		assertSafeFactSegment(t, store, segment, string(fact.ID), "")
		assertFactPathExists(t, ctx, ws, "facts/"+segment+".json")
	})
}

func TestLedgerWorkspaceStoreMetadataJSONRoundTripSemantics(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	fact := validFact("fact-1")
	fact.Metadata = map[string]any{
		"int":  7,
		"bool": true,
		"nested": map[string]any{
			"count": 2,
			"items": []any{3, map[string]any{"inner": 4}},
		},
	}

	if _, err := store.Put(ctx, fact); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, "fact-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}

	if got.Metadata["int"] != float64(7) {
		t.Fatalf("metadata int = %#v, want float64(7)", got.Metadata["int"])
	}
	if got.Metadata["bool"] != true {
		t.Fatalf("metadata bool = %#v, want true", got.Metadata["bool"])
	}
	nested, ok := got.Metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested type = %T, want map[string]any", got.Metadata["nested"])
	}
	if nested["count"] != float64(2) {
		t.Fatalf("metadata nested count = %#v, want float64(2)", nested["count"])
	}
	items, ok := nested["items"].([]any)
	if !ok {
		t.Fatalf("metadata nested items type = %T, want []any", nested["items"])
	}
	if items[0] != float64(3) {
		t.Fatalf("metadata nested items[0] = %#v, want float64(3)", items[0])
	}
	inner, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested items[1] type = %T, want map[string]any", items[1])
	}
	if inner["inner"] != float64(4) {
		t.Fatalf("metadata nested inner = %#v, want float64(4)", inner["inner"])
	}
}

func TestLedgerWorkspaceStoreValidationErrors(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	invalidStatus := FactStatus("unknown")

	invalid := validFact("fact-1")
	invalid.Predicate = ""
	if _, err := store.Put(ctx, invalid); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put invalid fact error = %v, want validation", err)
	}
	if _, err := store.Put(ctx, validFact("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put empty id error = %v, want validation", err)
	}
	if _, _, err := store.Get(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty id error = %v, want validation", err)
	}
	if _, err := store.List(ctx, ListOptions{Status: &invalidStatus}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List invalid status error = %v, want validation", err)
	}
	if err := store.Delete(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty id error = %v, want validation", err)
	}
	if err := store.DeleteSubject(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteSubject empty subject error = %v, want validation", err)
	}
}

func assertFactEqual(t *testing.T, got, want Fact) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fact mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertFactIDs(t *testing.T, facts []Fact, want []FactID) {
	t.Helper()
	got := make([]FactID, 0, len(facts))
	for _, fact := range facts {
		got = append(got, fact.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fact IDs = %v, want %v", got, want)
	}
}

func assertSafeFactSegment(t *testing.T, store *LedgerWorkspaceStore, segment, raw, wantPrefix string) {
	t.Helper()
	if !strings.HasPrefix(segment, wantPrefix) {
		t.Fatalf("segment %q for raw %q missing %q prefix", segment, raw, wantPrefix)
	}
	if strings.Contains(segment, "/") || segment == "." || segment == ".." {
		t.Fatalf("segment %q for raw %q is not path safe", segment, raw)
	}
	decoded, err := store.rawPathSegment(segment)
	if err != nil {
		t.Fatalf("rawPathSegment(%q) error = %v", segment, err)
	}
	if decoded != raw {
		t.Fatalf("rawPathSegment(%q) = %q, want %q", segment, decoded, raw)
	}
}

func assertFactPathExists(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || !exists {
		t.Fatalf("path %q exists = %v err %v, want true nil", path, exists, err)
	}
}

func assertFactPathMissing(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || exists {
		t.Fatalf("path %q exists = %v err %v, want false nil", path, exists, err)
	}
}
