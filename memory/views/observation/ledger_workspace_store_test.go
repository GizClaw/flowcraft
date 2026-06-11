package observation

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

	if _, err := store.Put(ctx, validObservation("obs-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.Get(ctx, "obs-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil workspace error = %v, want validation", err)
	}
	if _, err := store.List(ctx, ListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil workspace error = %v, want validation", err)
	}
	if err := store.Delete(ctx, "obs-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteScope(ctx, validScope()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteScope nil workspace error = %v, want validation", err)
	}
}

func TestLedgerWorkspaceStorePutGetListDeepClone(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	observation := validObservation("obs-1")

	put, err := store.Put(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}

	observation.SourceRefs[0].Message.MessageID = "mutated-input"
	observation.Signature.SourceRevisions[0].Revision = "mutated-input"
	observation.Signature.DiagnosticSignatures["extractor"] = "mutated-input"
	observation.Metadata["k"] = "mutated-input"
	setNestedMetadata(observation.Metadata, "mutated-input")

	put.SourceRefs[0].Message.MessageID = "mutated-put"
	put.Signature.SourceRevisions[0].Revision = "mutated-put"
	put.Signature.DiagnosticSignatures["extractor"] = "mutated-put"
	put.Metadata["k"] = "mutated-put"
	setNestedMetadata(put.Metadata, "mutated-put")

	got, ok, err := store.Get(ctx, "obs-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	assertObservationEqual(t, got, validObservation("obs-1"))

	got.SourceRefs[0].Message.MessageID = "mutated-get"
	got.Signature.SourceRevisions[0].Revision = "mutated-get"
	got.Signature.DiagnosticSignatures["extractor"] = "mutated-get"
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")

	listed, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List returned %d observations, want 1", len(listed))
	}
	listed[0].SourceRefs[0].Message.MessageID = "mutated-list"
	listed[0].Signature.SourceRevisions[0].Revision = "mutated-list"
	listed[0].Signature.DiagnosticSignatures["extractor"] = "mutated-list"
	listed[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listed[0].Metadata, "mutated-list")

	again, ok, err := store.Get(ctx, "obs-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get after List mutation ok = false, want true")
	}
	assertObservationEqual(t, again, validObservation("obs-1"))
}

func TestLedgerWorkspaceStoreListOrderAfterIDLimitScopeAndSubject(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	scopeOne := validScope()
	scopeTwo := Scope{RuntimeID: "runtime-1", UserID: "user-1", AgentID: "agent-1", ConversationID: "conversation-2", DatasetID: "dataset-1", EntityID: "user-123"}

	observations := map[string]Observation{
		"bravo":   validObservation("bravo"),
		"alpha":   validObservation("alpha"),
		"delta":   validObservation("delta"),
		"charlie": validObservation("charlie"),
	}
	bravo := observations["bravo"]
	bravo.Subject = "user:456"
	observations["bravo"] = bravo
	charlie := observations["charlie"]
	charlie.Scope = scopeTwo
	observations["charlie"] = charlie

	for _, id := range []string{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.Put(ctx, observations[id]); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, all, []string{"alpha", "bravo", "charlie", "delta"})

	afterLimited, err := store.List(ctx, ListOptions{AfterID: "alpha", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, afterLimited, []string{"bravo", "charlie"})

	scopeFiltered, err := store.List(ctx, ListOptions{Scope: &scopeOne})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, scopeFiltered, []string{"alpha", "bravo", "delta"})

	subjectFiltered, err := store.List(ctx, ListOptions{Subject: "user:123"})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, subjectFiltered, []string{"alpha", "charlie", "delta"})

	scopeSubjectLimited, err := store.List(ctx, ListOptions{
		AfterID: "alpha",
		Limit:   2,
		Scope:   &scopeOne,
		Subject: "user:123",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, scopeSubjectLimited, []string{"delta"})
}

func TestLedgerWorkspaceStoreDeleteOneObservation(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())

	if _, err := store.Put(ctx, validObservation("obs-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, validObservation("obs-2")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "obs-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "obs-1"); err != nil || ok {
		t.Fatalf("Get deleted observation ok = %v err %v, want false nil", ok, err)
	}
	if err := store.Delete(ctx, "obs-1"); err != nil {
		t.Fatalf("second Delete error = %v, want nil", err)
	}
	if got, ok, err := store.Get(ctx, "obs-2"); err != nil || !ok || got.ID != "obs-2" {
		t.Fatalf("Get kept observation = %+v ok %v err %v, want obs-2 true nil", got, ok, err)
	}
}

func TestLedgerWorkspaceStoreDeleteScopeOnlyDeletesMatchingScope(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	scopeOne := validScope()
	scopeTwo := Scope{RuntimeID: "runtime-1", UserID: "user-1", AgentID: "agent-1", ConversationID: "conversation-2", DatasetID: "dataset-1", EntityID: "user-123"}

	obsOne := validObservation("obs-1")
	obsTwo := validObservation("obs-2")
	obsTwo.Scope = scopeTwo
	obsThree := validObservation("obs-3")

	for _, observation := range []Observation{obsOne, obsTwo, obsThree} {
		if _, err := store.Put(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteScope(ctx, scopeOne); err != nil {
		t.Fatal(err)
	}

	listed, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertObservationIDs(t, listed, []string{"obs-2"})
	if listed[0].Scope != scopeTwo {
		t.Fatalf("remaining scope = %+v, want %+v", listed[0].Scope, scopeTwo)
	}

	if err := store.DeleteScope(ctx, scopeOne); err != nil {
		t.Fatalf("second DeleteScope error = %v, want nil", err)
	}
}

func TestLedgerWorkspaceStorePathSegmentPrefixDefaultCustomAndExplicitEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("default", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewLedgerWorkspaceStore(ws)
		observation := validObservation("obs/with/slash")
		if _, err := store.Put(ctx, observation); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(observation.ID)
		assertSafeObservationSegment(t, store, segment, observation.ID, "obs_")
		assertObservationPathExists(t, ctx, ws, "observations/"+segment+".json")
		assertObservationPathMissing(t, ctx, ws, "observations/"+observation.ID+".json")
	})

	t.Run("custom", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewLedgerWorkspaceStore(ws, WithLedgerPathSegmentPrefix("custom_"))
		observation := validObservation("../obs.json")
		if _, err := store.Put(ctx, observation); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(observation.ID)
		assertSafeObservationSegment(t, store, segment, observation.ID, "custom_")
		assertObservationPathExists(t, ctx, ws, "observations/"+segment+".json")
		assertObservationPathMissing(t, ctx, ws, "observations/"+observation.ID+".json")
	})

	t.Run("explicit empty", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewLedgerWorkspaceStore(ws, WithLedgerPathSegmentPrefix(""))
		observation := validObservation("obs/with/slash")
		if _, err := store.Put(ctx, observation); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(observation.ID)
		if strings.HasPrefix(segment, defaultLedgerPathSegmentPrefix) {
			t.Fatalf("explicit empty prefix segment = %q, should not use default prefix", segment)
		}
		assertSafeObservationSegment(t, store, segment, observation.ID, "")
		assertObservationPathExists(t, ctx, ws, "observations/"+segment+".json")
	})
}

func TestLedgerWorkspaceStoreMetadataJSONRoundTripSemantics(t *testing.T) {
	ctx := context.Background()
	store := NewLedgerWorkspaceStore(workspace.NewMemWorkspace())
	observation := validObservation("obs-1")
	observation.Metadata = map[string]any{
		"int":  7,
		"bool": true,
		"nested": map[string]any{
			"count": 2,
			"items": []any{3, map[string]any{"inner": 4}},
		},
	}

	if _, err := store.Put(ctx, observation); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, "obs-1")
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

	invalid := validObservation("obs-1")
	invalid.Predicate = ""
	if _, err := store.Put(ctx, invalid); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put invalid observation error = %v, want validation", err)
	}
	if _, _, err := store.Get(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty id error = %v, want validation", err)
	}
	if _, err := store.List(ctx, ListOptions{Scope: &Scope{UserID: "user-1"}}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List invalid scope error = %v, want validation", err)
	}
	if err := store.Delete(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty id error = %v, want validation", err)
	}
	if err := store.DeleteScope(ctx, Scope{ConversationID: "conversation-1"}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteScope invalid scope error = %v, want validation", err)
	}
}

func assertObservationEqual(t *testing.T, got, want Observation) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("observation mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertObservationIDs(t *testing.T, observations []Observation, want []string) {
	t.Helper()
	got := make([]string, 0, len(observations))
	for _, observation := range observations {
		got = append(got, observation.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("observation IDs = %v, want %v", got, want)
	}
}

func assertSafeObservationSegment(t *testing.T, store *LedgerWorkspaceStore, segment, raw, wantPrefix string) {
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

func assertObservationPathExists(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || !exists {
		t.Fatalf("path %q exists = %v err %v, want true nil", path, exists, err)
	}
}

func assertObservationPathMissing(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || exists {
		t.Fatalf("path %q exists = %v err %v, want false nil", path, exists, err)
	}
}
