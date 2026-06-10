package entity

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestProfileWorkspaceStoreNilWorkspaceReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	store := NewProfileWorkspaceStore(nil)

	if _, err := store.Put(ctx, validProfileRecord("profile-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.Get(ctx, "profile-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil workspace error = %v, want validation", err)
	}
	if _, err := store.List(ctx, ProfileListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil workspace error = %v, want validation", err)
	}
	if err := store.Delete(ctx, "profile-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteEntity(ctx, "entity-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity nil workspace error = %v, want validation", err)
	}
}

func TestTimelineWorkspaceStoreNilWorkspaceReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	store := NewTimelineWorkspaceStore(nil)

	if _, err := store.Put(ctx, validEvent("event-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil workspace error = %v, want validation", err)
	}
	if _, _, err := store.Get(ctx, "event-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil workspace error = %v, want validation", err)
	}
	if _, err := store.List(ctx, TimelineListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil workspace error = %v, want validation", err)
	}
	if err := store.Delete(ctx, "event-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil workspace error = %v, want validation", err)
	}
	if err := store.DeleteEntity(ctx, "entity-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity nil workspace error = %v, want validation", err)
	}
}

func TestProfileWorkspaceStorePutGetDeepClone(t *testing.T) {
	ctx := context.Background()
	store := NewProfileWorkspaceStore(workspace.NewMemWorkspace())

	record := validProfileRecord("profile-1")
	put, err := store.Put(ctx, record)
	if err != nil {
		t.Fatal(err)
	}

	record.Slots[0].Value = "mutated-input"
	record.Slots[0].FactRefs[0].FactID = "mutated-input"
	record.FactRefs[0].FactID = "mutated-input"
	record.SourceRefs[0].Message.MessageID = "mutated-input"
	record.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-input"
	record.Signature.DiagnosticSignatures["projector"] = "mutated-input"
	record.Metadata["k"] = "mutated-input"
	setNestedMetadata(record.Metadata, "mutated-input")
	setNestedMetadata(record.Slots[0].Metadata, "mutated-input")

	put.Slots[0].Value = "mutated-put"
	put.FactRefs[0].FactID = "mutated-put"
	put.Metadata["k"] = "mutated-put"
	setNestedMetadata(put.Metadata, "mutated-put")

	want := validProfileRecord("profile-1")
	got, ok, err := store.Get(ctx, "profile-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	assertProfileEqual(t, got, want)

	got.Slots[0].Value = "mutated-get"
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	again, ok, err := store.Get(ctx, "profile-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get after mutation ok = false, want true")
	}
	assertProfileEqual(t, again, want)
}

func TestTimelineWorkspaceStorePutGetDeepClone(t *testing.T) {
	ctx := context.Background()
	store := NewTimelineWorkspaceStore(workspace.NewMemWorkspace())

	event := validEvent("event-1")
	put, err := store.Put(ctx, event)
	if err != nil {
		t.Fatal(err)
	}

	event.FactRefs[0].FactID = "mutated-input"
	event.SourceRefs[0].Message.MessageID = "mutated-input"
	event.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-input"
	event.Signature.DiagnosticSignatures["projector"] = "mutated-input"
	event.Metadata["k"] = "mutated-input"
	setNestedMetadata(event.Metadata, "mutated-input")
	*event.OccurredAt = event.OccurredAt.AddDate(0, 0, 1)
	*event.ValidFrom = event.ValidFrom.AddDate(0, 0, 1)

	put.FactRefs[0].FactID = "mutated-put"
	put.Metadata["k"] = "mutated-put"
	setNestedMetadata(put.Metadata, "mutated-put")
	*put.OccurredAt = put.OccurredAt.AddDate(0, 0, 1)
	*put.ValidFrom = put.ValidFrom.AddDate(0, 0, 1)

	want := validEvent("event-1")
	got, ok, err := store.Get(ctx, "event-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	assertEventEqual(t, got, want)

	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	*got.OccurredAt = got.OccurredAt.AddDate(0, 0, 1)
	again, ok, err := store.Get(ctx, "event-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get after mutation ok = false, want true")
	}
	assertEventEqual(t, again, want)
}

func TestProfileWorkspaceStoreListOrderAfterIDLimitAndFilters(t *testing.T) {
	ctx := context.Background()
	store := NewProfileWorkspaceStore(workspace.NewMemWorkspace())

	records := map[ProfileID]ProfileRecord{
		"bravo":   validProfileRecord("bravo"),
		"alpha":   validProfileRecord("alpha"),
		"delta":   validProfileRecord("delta"),
		"charlie": validProfileRecord("charlie"),
	}
	bravo := records["bravo"]
	bravo.EntityID = "entity-2"
	records["bravo"] = bravo
	charlie := records["charlie"]
	charlie.Label = "Other"
	records["charlie"] = charlie

	for _, id := range []ProfileID{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.Put(ctx, records[id]); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List(ctx, ProfileListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertProfileIDs(t, all, []ProfileID{"alpha", "bravo", "charlie", "delta"})

	afterLimited, err := store.List(ctx, ProfileListOptions{AfterID: "alpha", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertProfileIDs(t, afterLimited, []ProfileID{"bravo", "charlie"})

	entityFiltered, err := store.List(ctx, ProfileListOptions{EntityID: "entity-1"})
	if err != nil {
		t.Fatal(err)
	}
	assertProfileIDs(t, entityFiltered, []ProfileID{"alpha", "charlie", "delta"})

	labelFiltered, err := store.List(ctx, ProfileListOptions{Label: "Hai"})
	if err != nil {
		t.Fatal(err)
	}
	assertProfileIDs(t, labelFiltered, []ProfileID{"alpha", "bravo", "delta"})

	combined, err := store.List(ctx, ProfileListOptions{
		Limit:    2,
		EntityID: "entity-1",
		Label:    "Hai",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProfileIDs(t, combined, []ProfileID{"alpha", "delta"})
}

func TestTimelineWorkspaceStoreListOrderAfterIDLimitAndFilters(t *testing.T) {
	ctx := context.Background()
	store := NewTimelineWorkspaceStore(workspace.NewMemWorkspace())

	events := map[EventID]Event{
		"bravo":   validEvent("bravo"),
		"alpha":   validEvent("alpha"),
		"delta":   validEvent("delta"),
		"charlie": validEvent("charlie"),
	}
	bravo := events["bravo"]
	bravo.EntityID = "entity-2"
	events["bravo"] = bravo

	for _, id := range []EventID{"bravo", "alpha", "delta", "charlie"} {
		if _, err := store.Put(ctx, events[id]); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.List(ctx, TimelineListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertEventIDs(t, all, []EventID{"alpha", "bravo", "charlie", "delta"})

	afterLimited, err := store.List(ctx, TimelineListOptions{AfterID: "alpha", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertEventIDs(t, afterLimited, []EventID{"bravo", "charlie"})

	entityFiltered, err := store.List(ctx, TimelineListOptions{EntityID: "entity-1"})
	if err != nil {
		t.Fatal(err)
	}
	assertEventIDs(t, entityFiltered, []EventID{"alpha", "charlie", "delta"})

	combined, err := store.List(ctx, TimelineListOptions{
		Limit:    2,
		EntityID: "entity-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEventIDs(t, combined, []EventID{"alpha", "charlie"})
}

func TestProfileWorkspaceStoreDelete(t *testing.T) {
	ctx := context.Background()
	store := NewProfileWorkspaceStore(workspace.NewMemWorkspace())

	if _, err := store.Put(ctx, validProfileRecord("profile-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, validProfileRecord("profile-2")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "profile-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "profile-1"); err != nil || ok {
		t.Fatalf("Get deleted profile ok = %v err %v, want false nil", ok, err)
	}
	if err := store.Delete(ctx, "profile-1"); err != nil {
		t.Fatalf("second Delete error = %v, want nil", err)
	}
	if got, ok, err := store.Get(ctx, "profile-2"); err != nil || !ok || got.ID != "profile-2" {
		t.Fatalf("Get kept profile = %+v ok %v err %v, want profile-2 true nil", got, ok, err)
	}
}

func TestTimelineWorkspaceStoreDelete(t *testing.T) {
	ctx := context.Background()
	store := NewTimelineWorkspaceStore(workspace.NewMemWorkspace())

	if _, err := store.Put(ctx, validEvent("event-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, validEvent("event-2")); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "event-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, "event-1"); err != nil || ok {
		t.Fatalf("Get deleted event ok = %v err %v, want false nil", ok, err)
	}
	if err := store.Delete(ctx, "event-1"); err != nil {
		t.Fatalf("second Delete error = %v, want nil", err)
	}
	if got, ok, err := store.Get(ctx, "event-2"); err != nil || !ok || got.ID != "event-2" {
		t.Fatalf("Get kept event = %+v ok %v err %v, want event-2 true nil", got, ok, err)
	}
}

func TestProfileWorkspaceStoreDeleteEntityOnlyDeletesMatchingEntity(t *testing.T) {
	ctx := context.Background()
	store := NewProfileWorkspaceStore(workspace.NewMemWorkspace())

	one := validProfileRecord("profile-1")
	two := validProfileRecord("profile-2")
	two.EntityID = "entity-2"
	three := validProfileRecord("profile-3")
	for _, record := range []ProfileRecord{one, two, three} {
		if _, err := store.Put(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteEntity(ctx, "entity-1"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx, ProfileListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertProfileIDs(t, listed, []ProfileID{"profile-2"})
	if listed[0].EntityID != "entity-2" {
		t.Fatalf("remaining entity = %q, want entity-2", listed[0].EntityID)
	}
	if err := store.DeleteEntity(ctx, "entity-1"); err != nil {
		t.Fatalf("second DeleteEntity error = %v, want nil", err)
	}
}

func TestTimelineWorkspaceStoreDeleteEntityOnlyDeletesMatchingEntity(t *testing.T) {
	ctx := context.Background()
	store := NewTimelineWorkspaceStore(workspace.NewMemWorkspace())

	one := validEvent("event-1")
	two := validEvent("event-2")
	two.EntityID = "entity-2"
	three := validEvent("event-3")
	for _, event := range []Event{one, two, three} {
		if _, err := store.Put(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteEntity(ctx, "entity-1"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx, TimelineListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertEventIDs(t, listed, []EventID{"event-2"})
	if listed[0].EntityID != "entity-2" {
		t.Fatalf("remaining entity = %q, want entity-2", listed[0].EntityID)
	}
	if err := store.DeleteEntity(ctx, "entity-1"); err != nil {
		t.Fatalf("second DeleteEntity error = %v, want nil", err)
	}
}

func TestProfileWorkspaceStorePathSegmentPrefixDefaultCustomAndExplicitEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("default", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewProfileWorkspaceStore(ws)
		record := validProfileRecord("profile/with/slash")
		if _, err := store.Put(ctx, record); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(record.ID))
		assertSafeProfileSegment(t, store, segment, string(record.ID), "eprof_")
		assertPathExists(t, ctx, ws, "entity/profiles/"+segment+".json")
		assertPathMissing(t, ctx, ws, "entity/profiles/"+string(record.ID)+".json")
	})

	t.Run("custom", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewProfileWorkspaceStore(ws, WithProfilePathSegmentPrefix("custom_"))
		record := validProfileRecord("../profile.json")
		if _, err := store.Put(ctx, record); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(record.ID))
		assertSafeProfileSegment(t, store, segment, string(record.ID), "custom_")
		assertPathExists(t, ctx, ws, "entity/profiles/"+segment+".json")
		assertPathMissing(t, ctx, ws, "entity/profiles/"+string(record.ID)+".json")
	})

	t.Run("explicit empty", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewProfileWorkspaceStore(ws, WithProfilePathSegmentPrefix(""))
		record := validProfileRecord("profile/with/slash")
		if _, err := store.Put(ctx, record); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(record.ID))
		if strings.HasPrefix(segment, defaultProfilePathSegmentPrefix) {
			t.Fatalf("explicit empty prefix segment = %q, should not use default prefix", segment)
		}
		assertSafeProfileSegment(t, store, segment, string(record.ID), "")
		assertPathExists(t, ctx, ws, "entity/profiles/"+segment+".json")
	})
}

func TestTimelineWorkspaceStorePathSegmentPrefixDefaultCustomAndExplicitEmpty(t *testing.T) {
	ctx := context.Background()

	t.Run("default", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewTimelineWorkspaceStore(ws)
		event := validEvent("event/with/slash")
		if _, err := store.Put(ctx, event); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(event.ID))
		assertSafeTimelineSegment(t, store, segment, string(event.ID), "etl_")
		assertPathExists(t, ctx, ws, "entity/timeline/"+segment+".json")
		assertPathMissing(t, ctx, ws, "entity/timeline/"+string(event.ID)+".json")
	})

	t.Run("custom", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewTimelineWorkspaceStore(ws, WithTimelinePathSegmentPrefix("custom_"))
		event := validEvent("../event.json")
		if _, err := store.Put(ctx, event); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(event.ID))
		assertSafeTimelineSegment(t, store, segment, string(event.ID), "custom_")
		assertPathExists(t, ctx, ws, "entity/timeline/"+segment+".json")
		assertPathMissing(t, ctx, ws, "entity/timeline/"+string(event.ID)+".json")
	})

	t.Run("explicit empty", func(t *testing.T) {
		ws := workspace.NewMemWorkspace()
		store := NewTimelineWorkspaceStore(ws, WithTimelinePathSegmentPrefix(""))
		event := validEvent("event/with/slash")
		if _, err := store.Put(ctx, event); err != nil {
			t.Fatal(err)
		}

		segment := store.pathSegment(string(event.ID))
		if strings.HasPrefix(segment, defaultTimelinePathSegmentPrefix) {
			t.Fatalf("explicit empty prefix segment = %q, should not use default prefix", segment)
		}
		assertSafeTimelineSegment(t, store, segment, string(event.ID), "")
		assertPathExists(t, ctx, ws, "entity/timeline/"+segment+".json")
	})
}

func TestEntityWorkspaceStoreMetadataJSONRoundTripSemantics(t *testing.T) {
	ctx := context.Background()
	profileStore := NewProfileWorkspaceStore(workspace.NewMemWorkspace())
	timelineStore := NewTimelineWorkspaceStore(workspace.NewMemWorkspace())
	metadata := map[string]any{
		"int":  7,
		"bool": true,
		"nested": map[string]any{
			"count": 2,
			"items": []any{3, map[string]any{"inner": 4}},
		},
	}

	record := validProfileRecord("profile-1")
	record.Metadata = cloneAnyMap(metadata)
	record.Slots[0].Metadata = cloneAnyMap(metadata)
	if _, err := profileStore.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	gotRecord, ok, err := profileStore.Get(ctx, "profile-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get profile ok = false, want true")
	}
	assertJSONMetadataSemantics(t, gotRecord.Metadata)
	assertJSONMetadataSemantics(t, gotRecord.Slots[0].Metadata)

	event := validEvent("event-1")
	event.Metadata = cloneAnyMap(metadata)
	if _, err := timelineStore.Put(ctx, event); err != nil {
		t.Fatal(err)
	}
	gotEvent, ok, err := timelineStore.Get(ctx, "event-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get event ok = false, want true")
	}
	assertJSONMetadataSemantics(t, gotEvent.Metadata)
}

func TestEntityWorkspaceStoreValidationErrors(t *testing.T) {
	ctx := context.Background()
	profileStore := NewProfileWorkspaceStore(workspace.NewMemWorkspace())
	timelineStore := NewTimelineWorkspaceStore(workspace.NewMemWorkspace())

	invalidProfile := validProfileRecord("profile-1")
	invalidProfile.Label = ""
	if _, err := profileStore.Put(ctx, invalidProfile); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put invalid profile error = %v, want validation", err)
	}
	if _, err := profileStore.Put(ctx, validProfileRecord("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put empty profile id error = %v, want validation", err)
	}
	if _, _, err := profileStore.Get(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty profile id error = %v, want validation", err)
	}
	if err := profileStore.Delete(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty profile id error = %v, want validation", err)
	}
	if err := profileStore.DeleteEntity(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity empty entity error = %v, want validation", err)
	}

	invalidEvent := validEvent("event-1")
	invalidEvent.Title = ""
	if _, err := timelineStore.Put(ctx, invalidEvent); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put invalid event error = %v, want validation", err)
	}
	if _, err := timelineStore.Put(ctx, validEvent("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put empty event id error = %v, want validation", err)
	}
	if _, _, err := timelineStore.Get(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty event id error = %v, want validation", err)
	}
	if err := timelineStore.Delete(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty event id error = %v, want validation", err)
	}
	if err := timelineStore.DeleteEntity(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity empty entity error = %v, want validation", err)
	}
}

func assertProfileEqual(t *testing.T, got, want ProfileRecord) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profile mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertEventEqual(t *testing.T, got, want Event) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event mismatch:\ngot  = %#v\nwant = %#v", got, want)
	}
}

func assertProfileIDs(t *testing.T, records []ProfileRecord, want []ProfileID) {
	t.Helper()
	got := make([]ProfileID, 0, len(records))
	for _, record := range records {
		got = append(got, record.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("profile IDs = %v, want %v", got, want)
	}
}

func assertEventIDs(t *testing.T, events []Event, want []EventID) {
	t.Helper()
	got := make([]EventID, 0, len(events))
	for _, event := range events {
		got = append(got, event.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("event IDs = %v, want %v", got, want)
	}
}

func assertSafeProfileSegment(t *testing.T, store *ProfileWorkspaceStore, segment, raw, wantPrefix string) {
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

func assertSafeTimelineSegment(t *testing.T, store *TimelineWorkspaceStore, segment, raw, wantPrefix string) {
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

func assertPathExists(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || !exists {
		t.Fatalf("path %q exists = %v err %v, want true nil", path, exists, err)
	}
}

func assertPathMissing(t *testing.T, ctx context.Context, ws workspace.Workspace, path string) {
	t.Helper()
	if exists, err := ws.Exists(ctx, path); err != nil || exists {
		t.Fatalf("path %q exists = %v err %v, want false nil", path, exists, err)
	}
}

func assertJSONMetadataSemantics(t *testing.T, metadata map[string]any) {
	t.Helper()

	if metadata["int"] != float64(7) {
		t.Fatalf("metadata int = %#v, want float64(7)", metadata["int"])
	}
	if metadata["bool"] != true {
		t.Fatalf("metadata bool = %#v, want true", metadata["bool"])
	}
	nested, ok := metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested type = %T, want map[string]any", metadata["nested"])
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
