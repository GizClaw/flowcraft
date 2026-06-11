package entity

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestProfileDescriptorDefaultsAndOptions(t *testing.T) {
	profile := NewProfile(nil)

	got := profile.Descriptor()
	if got.ID != DefaultProfileID {
		t.Fatalf("Descriptor ID = %q, want %q", got.ID, DefaultProfileID)
	}
	if got.Kind != views.KindEntityProfile {
		t.Fatalf("Descriptor Kind = %q, want %q", got.Kind, views.KindEntityProfile)
	}
	if got.Version != DefaultProfileVersion {
		t.Fatalf("Descriptor Version = %q, want %q", got.Version, DefaultProfileVersion)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor Validate() error = %v", err)
	}

	profile = NewProfile(nil, WithProfileID("project-profile"), WithProfileVersion("v-test"))
	got = profile.Descriptor()
	want := views.Descriptor{
		ID:      "project-profile",
		Kind:    views.KindEntityProfile,
		Version: "v-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptor = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("custom Descriptor Validate() error = %v", err)
	}
}

func TestTimelineDescriptorDefaultsAndOptions(t *testing.T) {
	timeline := NewTimeline(nil)

	got := timeline.Descriptor()
	if got.ID != DefaultTimelineID {
		t.Fatalf("Descriptor ID = %q, want %q", got.ID, DefaultTimelineID)
	}
	if got.Kind != views.KindEntityTimeline {
		t.Fatalf("Descriptor Kind = %q, want %q", got.Kind, views.KindEntityTimeline)
	}
	if got.Version != DefaultTimelineVersion {
		t.Fatalf("Descriptor Version = %q, want %q", got.Version, DefaultTimelineVersion)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor Validate() error = %v", err)
	}

	timeline = NewTimeline(nil, WithTimelineID("project-timeline"), WithTimelineVersion("v-test"))
	got = timeline.Descriptor()
	want := views.Descriptor{
		ID:      "project-timeline",
		Kind:    views.KindEntityTimeline,
		Version: "v-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptor = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("custom Descriptor Validate() error = %v", err)
	}
}

func TestProfileNilStoreReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	profile := NewProfile(nil)
	scope := testEntityScope("entity-1")

	if _, err := profile.Put(ctx, validProfileRecord("profile-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil store error = %v, want validation", err)
	}
	if _, _, err := profile.Get(ctx, scope, "profile-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil store error = %v, want validation", err)
	}
	if _, err := profile.List(ctx, ProfileListOptions{Scope: &scope}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil store error = %v, want validation", err)
	}
	if err := profile.Delete(ctx, scope, "profile-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil store error = %v, want validation", err)
	}
	if err := profile.DeleteEntity(ctx, scope); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity nil store error = %v, want validation", err)
	}
}

func TestTimelineNilStoreReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	timeline := NewTimeline(nil)
	scope := testEntityScope("entity-1")

	if _, err := timeline.Put(ctx, validEvent("event-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil store error = %v, want validation", err)
	}
	if _, _, err := timeline.Get(ctx, scope, "event-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil store error = %v, want validation", err)
	}
	if _, err := timeline.List(ctx, TimelineListOptions{Scope: &scope}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil store error = %v, want validation", err)
	}
	if err := timeline.Delete(ctx, scope, "event-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil store error = %v, want validation", err)
	}
	if err := timeline.DeleteEntity(ctx, scope); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity nil store error = %v, want validation", err)
	}
}

func TestProfilePutValidation(t *testing.T) {
	ctx := context.Background()
	profile := NewProfile(&fakeProfileStore{})

	tests := []struct {
		name   string
		mutate func(*ProfileRecord)
	}{
		{
			name: "missing id",
			mutate: func(record *ProfileRecord) {
				record.ID = ""
			},
		},
		{
			name: "missing entity",
			mutate: func(record *ProfileRecord) {
				record.Scope.EntityID = ""
			},
		},
		{
			name: "missing label",
			mutate: func(record *ProfileRecord) {
				record.Label = ""
			},
		},
		{
			name: "missing fact refs",
			mutate: func(record *ProfileRecord) {
				record.FactRefs = nil
			},
		},
		{
			name: "missing fact id",
			mutate: func(record *ProfileRecord) {
				record.FactRefs[0].FactID = ""
			},
		},
		{
			name: "missing signature",
			mutate: func(record *ProfileRecord) {
				record.Signature = views.ViewSignature{}
			},
		},
		{
			name: "missing upstream refs",
			mutate: func(record *ProfileRecord) {
				record.Signature.UpstreamViewRefs = nil
			},
		},
		{
			name: "upstream ref missing view id",
			mutate: func(record *ProfileRecord) {
				record.Signature.UpstreamViewRefs[0].ViewID = ""
			},
		},
		{
			name: "invalid source ref",
			mutate: func(record *ProfileRecord) {
				record.SourceRefs[0].Message.MessageID = ""
			},
		},
		{
			name: "missing slot name",
			mutate: func(record *ProfileRecord) {
				record.Slots[0].Name = ""
			},
		},
		{
			name: "missing slot value",
			mutate: func(record *ProfileRecord) {
				record.Slots[0].Value = ""
			},
		},
		{
			name: "invalid negative slot confidence",
			mutate: func(record *ProfileRecord) {
				record.Slots[0].Confidence = -0.1
			},
		},
		{
			name: "invalid high slot confidence",
			mutate: func(record *ProfileRecord) {
				record.Slots[0].Confidence = 1.1
			},
		},
		{
			name: "slot fact ref missing fact id",
			mutate: func(record *ProfileRecord) {
				record.Slots[0].FactRefs[0].FactID = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := validProfileRecord("profile-1")
			tt.mutate(&record)

			if _, err := profile.Put(ctx, record); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Put(%s) error = %v, want validation", tt.name, err)
			}
		})
	}
}

func TestTimelinePutValidation(t *testing.T) {
	ctx := context.Background()
	timeline := NewTimeline(&fakeTimelineStore{})

	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{
			name: "missing id",
			mutate: func(event *Event) {
				event.ID = ""
			},
		},
		{
			name: "missing entity",
			mutate: func(event *Event) {
				event.Scope.EntityID = ""
			},
		},
		{
			name: "missing title",
			mutate: func(event *Event) {
				event.Title = ""
			},
		},
		{
			name: "missing fact refs",
			mutate: func(event *Event) {
				event.FactRefs = nil
			},
		},
		{
			name: "missing fact id",
			mutate: func(event *Event) {
				event.FactRefs[0].FactID = ""
			},
		},
		{
			name: "missing signature",
			mutate: func(event *Event) {
				event.Signature = views.ViewSignature{}
			},
		},
		{
			name: "missing upstream refs",
			mutate: func(event *Event) {
				event.Signature.UpstreamViewRefs = nil
			},
		},
		{
			name: "upstream ref missing view id",
			mutate: func(event *Event) {
				event.Signature.UpstreamViewRefs[0].ViewID = ""
			},
		},
		{
			name: "invalid source ref",
			mutate: func(event *Event) {
				event.SourceRefs[0].Message.MessageID = ""
			},
		},
		{
			name: "invalid validity range",
			mutate: func(event *Event) {
				from := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
				until := from.Add(-time.Minute)
				event.ValidFrom = &from
				event.ValidUntil = &until
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := validEvent("event-1")
			tt.mutate(&event)

			if _, err := timeline.Put(ctx, event); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Put(%s) error = %v, want validation", tt.name, err)
			}
		})
	}
}

func TestProfileGetListDeleteValidation(t *testing.T) {
	ctx := context.Background()
	profile := NewProfile(&fakeProfileStore{})
	scope := testEntityScope("entity-1")

	if _, _, err := profile.Get(ctx, scope, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty id error = %v, want validation", err)
	}
	if err := profile.Delete(ctx, scope, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty id error = %v, want validation", err)
	}
	if err := profile.DeleteEntity(ctx, testEntityScope("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity empty entity error = %v, want validation", err)
	}
}

func TestTimelineGetListDeleteValidation(t *testing.T) {
	ctx := context.Background()
	timeline := NewTimeline(&fakeTimelineStore{})
	scope := testEntityScope("entity-1")

	if _, _, err := timeline.Get(ctx, scope, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty id error = %v, want validation", err)
	}
	if err := timeline.Delete(ctx, scope, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty id error = %v, want validation", err)
	}
	if err := timeline.DeleteEntity(ctx, testEntityScope("")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteEntity empty entity error = %v, want validation", err)
	}
}

func TestProfileDelegatesAndClonesBoundaries(t *testing.T) {
	ctx := context.Background()
	store := &fakeProfileStore{
		putOut:  validProfileRecord("profile-put-out"),
		getOut:  validProfileRecord("profile-get-out"),
		getOK:   true,
		listOut: []ProfileRecord{validProfileRecord("profile-list-out")},
	}
	profile := NewProfile(store)
	scope := testEntityScope("entity-1")

	input := validProfileRecord("profile-put-in")
	put, err := profile.Put(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if store.putIn.ID != input.ID || store.putIn.Scope != input.Scope {
		t.Fatalf("store Put received %+v, want delegated profile", store.putIn)
	}
	assertProfileMutableState(t, input, "display_name", "Hai", "fact-1", "fact-1", "message-1", "fact-output:v1", "profile:v1", "v", "v", "Put shared mutable state with caller")
	setNestedMetadata(store.putIn.Metadata, "mutated-store-captured-input")
	if input.Metadata["k"] != "v" {
		t.Fatalf("store input shared Metadata with caller; input metadata = %#v", input.Metadata)
	}
	assertNestedMetadata(t, input.Metadata, "v", "store input shared nested Metadata with caller")

	put.Slots[0].Value = "mutated-return"
	put.Slots[0].FactRefs[0].FactID = "mutated-return"
	put.FactRefs[0].FactID = "mutated-return"
	put.SourceRefs[0].Message.MessageID = "mutated-return"
	put.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-return"
	put.Signature.DiagnosticSignatures["projector"] = "mutated-return"
	put.Metadata["k"] = "mutated-return"
	setNestedMetadata(put.Metadata, "mutated-return")
	assertProfileMutableState(t, store.putOut, "display_name", "Hai", "fact-1", "fact-1", "message-1", "fact-output:v1", "profile:v1", "v", "v", "Put return shared mutable state with store output")

	got, ok, err := profile.Get(ctx, scope, "profile-get-out")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if store.getScope != scope || store.getID != "profile-get-out" {
		t.Fatalf("store Get args = %+v/%q, want scope/profile-get-out", store.getScope, store.getID)
	}
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	assertProfileMutableState(t, store.getOut, "display_name", "Hai", "fact-1", "fact-1", "message-1", "fact-output:v1", "profile:v1", "v", "v", "Get result shared mutable state with store output")

	listed, err := profile.List(ctx, ProfileListOptions{
		AfterID: "profile-a",
		Limit:   2,
		Scope:   &scope,
		Label:   "Hai",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.listOpts.AfterID != "profile-a" || store.listOpts.Limit != 2 || store.listOpts.Scope == nil || *store.listOpts.Scope != scope || store.listOpts.Label != "Hai" {
		t.Fatalf("store List options = %+v, want delegated options", store.listOpts)
	}
	listed[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listed[0].Metadata, "mutated-list")
	assertProfileMutableState(t, store.listOut[0], "display_name", "Hai", "fact-1", "fact-1", "message-1", "fact-output:v1", "profile:v1", "v", "v", "List result shared mutable state with store output")

	if err := profile.Delete(ctx, scope, "profile-delete"); err != nil {
		t.Fatal(err)
	}
	if store.deleteScope != scope || store.deleteID != "profile-delete" {
		t.Fatalf("store Delete args = %+v/%q, want scope/profile-delete", store.deleteScope, store.deleteID)
	}
	if err := profile.DeleteEntity(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if store.deleteEntityScope != scope {
		t.Fatalf("store DeleteEntity scope = %+v, want %+v", store.deleteEntityScope, scope)
	}
}

func TestTimelineDelegatesAndClonesBoundaries(t *testing.T) {
	ctx := context.Background()
	store := &fakeTimelineStore{
		putOut:  validEvent("event-put-out"),
		getOut:  validEvent("event-get-out"),
		getOK:   true,
		listOut: []Event{validEvent("event-list-out")},
	}
	timeline := NewTimeline(store)
	scope := testEntityScope("entity-1")

	input := validEvent("event-put-in")
	put, err := timeline.Put(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if store.putIn.ID != input.ID || store.putIn.Scope != input.Scope {
		t.Fatalf("store Put received %+v, want delegated event", store.putIn)
	}
	assertEventMutableState(t, input, "fact-1", "message-1", "fact-output:v1", "timeline:v1", "v", "v", "Put shared mutable state with caller")
	setNestedMetadata(store.putIn.Metadata, "mutated-store-captured-input")
	if input.Metadata["k"] != "v" {
		t.Fatalf("store input shared Metadata with caller; input metadata = %#v", input.Metadata)
	}
	assertNestedMetadata(t, input.Metadata, "v", "store input shared nested Metadata with caller")

	put.FactRefs[0].FactID = "mutated-return"
	put.SourceRefs[0].Message.MessageID = "mutated-return"
	put.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-return"
	put.Signature.DiagnosticSignatures["projector"] = "mutated-return"
	put.Metadata["k"] = "mutated-return"
	setNestedMetadata(put.Metadata, "mutated-return")
	*put.OccurredAt = put.OccurredAt.Add(time.Hour)
	*put.ValidFrom = put.ValidFrom.Add(time.Hour)
	assertEventMutableState(t, store.putOut, "fact-1", "message-1", "fact-output:v1", "timeline:v1", "v", "v", "Put return shared mutable state with store output")

	got, ok, err := timeline.Get(ctx, scope, "event-get-out")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if store.getScope != scope || store.getID != "event-get-out" {
		t.Fatalf("store Get args = %+v/%q, want scope/event-get-out", store.getScope, store.getID)
	}
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	assertEventMutableState(t, store.getOut, "fact-1", "message-1", "fact-output:v1", "timeline:v1", "v", "v", "Get result shared mutable state with store output")

	listed, err := timeline.List(ctx, TimelineListOptions{
		AfterID: "event-a",
		Limit:   2,
		Scope:   &scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.listOpts.AfterID != "event-a" || store.listOpts.Limit != 2 || store.listOpts.Scope == nil || *store.listOpts.Scope != scope {
		t.Fatalf("store List options = %+v, want delegated options", store.listOpts)
	}
	listed[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listed[0].Metadata, "mutated-list")
	assertEventMutableState(t, store.listOut[0], "fact-1", "message-1", "fact-output:v1", "timeline:v1", "v", "v", "List result shared mutable state with store output")

	if err := timeline.Delete(ctx, scope, "event-delete"); err != nil {
		t.Fatal(err)
	}
	if store.deleteScope != scope || store.deleteID != "event-delete" {
		t.Fatalf("store Delete args = %+v/%q, want scope/event-delete", store.deleteScope, store.deleteID)
	}
	if err := timeline.DeleteEntity(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if store.deleteEntityScope != scope {
		t.Fatalf("store DeleteEntity scope = %+v, want %+v", store.deleteEntityScope, scope)
	}
}

func validProfileRecord(id ProfileID) ProfileRecord {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	return ProfileRecord{
		ID:      id,
		Scope:   testEntityScope("entity-1"),
		Label:   "Hai",
		Summary: "Likes coffee and quiet mornings.",
		Slots: []Slot{{
			Name:       "display_name",
			Value:      "Hai",
			Confidence: 0.9,
			FactRefs: []fact.FactRef{{
				FactID: "fact-1",
				Role:   "slot",
			}},
			Metadata: validMetadata(),
		}},
		FactRefs: []fact.FactRef{{
			FactID: "fact-1",
			Role:   "profile",
		}},
		SourceRefs: []views.SourceRef{validSourceRef()},
		Signature:  validEntitySignature(DefaultProfileID, "profile:v1"),
		CreatedAt:  created,
		UpdatedAt:  updated,
		Metadata:   validMetadata(),
	}
}

func validEvent(id EventID) Event {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	occurredAt := time.Date(2026, 6, 3, 8, 0, 0, 0, time.UTC)
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	return Event{
		ID:          id,
		Scope:       testEntityScope("entity-1"),
		Title:       "Liked coffee",
		Description: "User preference was observed.",
		OccurredAt:  &occurredAt,
		ValidFrom:   &validFrom,
		ValidUntil:  &validUntil,
		FactRefs: []fact.FactRef{{
			FactID: "fact-1",
			Role:   "event",
		}},
		SourceRefs: []views.SourceRef{validSourceRef()},
		Signature:  validEntitySignature(DefaultTimelineID, "timeline:v1"),
		CreatedAt:  created,
		UpdatedAt:  updated,
		Metadata:   validMetadata(),
	}
}

func testEntityScope(entityID string) views.Scope {
	return views.Scope{RuntimeID: "runtime-1", UserID: "user-1", EntityID: entityID}
}

func validSourceRef() views.SourceRef {
	return views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: "conversation-1",
			MessageID:      "message-1",
			Span:           &views.Span{Start: 0, End: 10},
		},
	}
}

func validEntitySignature(viewID views.ID, diagnostic string) views.ViewSignature {
	return views.ViewSignature{
		ViewID: viewID,
		UpstreamViewRefs: []views.UpstreamViewRef{{
			ViewID:          views.ID("fact-graph"),
			OutputSignature: "fact-output:v1",
			RecordKey:       "entity-1",
		}},
		DiagnosticSignatures: map[string]string{"projector": diagnostic},
	}
}

func validMetadata() map[string]any {
	return map[string]any{
		"k": "v",
		"nested": map[string]any{
			"tag":   "v",
			"items": []any{"v", map[string]any{"inner": "v"}},
		},
	}
}

func setNestedMetadata(metadata map[string]any, value string) {
	nested := metadata["nested"].(map[string]any)
	nested["tag"] = value
	items := nested["items"].([]any)
	items[0] = value
	inner := items[1].(map[string]any)
	inner["inner"] = value
}

func assertNestedMetadata(t *testing.T, metadata map[string]any, want string, message string) {
	t.Helper()

	nested, ok := metadata["nested"].(map[string]any)
	if !ok {
		t.Fatalf("%s; nested metadata = %T", message, metadata["nested"])
	}
	if got := nested["tag"]; got != want {
		t.Fatalf("%s; nested tag = %q, want %q", message, got, want)
	}
	items, ok := nested["items"].([]any)
	if !ok {
		t.Fatalf("%s; nested items = %T", message, nested["items"])
	}
	if got := items[0]; got != want {
		t.Fatalf("%s; nested slice[0] = %q, want %q", message, got, want)
	}
	inner, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("%s; nested slice[1] = %T", message, items[1])
	}
	if got := inner["inner"]; got != want {
		t.Fatalf("%s; nested inner = %q, want %q", message, got, want)
	}
}

func assertProfileMutableState(t *testing.T, record ProfileRecord, slotName, slotValue string, slotFactID, factID fact.FactID, messageID, upstreamOutput, diagnostic, metadata, nestedMetadata, message string) {
	t.Helper()

	if record.Slots[0].Name != slotName {
		t.Fatalf("%s; slot name = %q, want %q", message, record.Slots[0].Name, slotName)
	}
	if record.Slots[0].Value != slotValue {
		t.Fatalf("%s; slot value = %q, want %q", message, record.Slots[0].Value, slotValue)
	}
	if record.Slots[0].FactRefs[0].FactID != slotFactID {
		t.Fatalf("%s; slot fact ref = %q, want %q", message, record.Slots[0].FactRefs[0].FactID, slotFactID)
	}
	if record.FactRefs[0].FactID != factID {
		t.Fatalf("%s; fact ref = %q, want %q", message, record.FactRefs[0].FactID, factID)
	}
	if record.SourceRefs[0].Message.MessageID != messageID {
		t.Fatalf("%s; source ref message id = %q, want %q", message, record.SourceRefs[0].Message.MessageID, messageID)
	}
	if record.Signature.UpstreamViewRefs[0].OutputSignature != upstreamOutput {
		t.Fatalf("%s; upstream output = %q, want %q", message, record.Signature.UpstreamViewRefs[0].OutputSignature, upstreamOutput)
	}
	if record.Signature.DiagnosticSignatures["projector"] != diagnostic {
		t.Fatalf("%s; diagnostic signature = %q, want %q", message, record.Signature.DiagnosticSignatures["projector"], diagnostic)
	}
	if record.Metadata["k"] != metadata {
		t.Fatalf("%s; metadata = %#v, want %q", message, record.Metadata["k"], metadata)
	}
	assertNestedMetadata(t, record.Metadata, nestedMetadata, message)
	assertNestedMetadata(t, record.Slots[0].Metadata, nestedMetadata, message)
}

func assertEventMutableState(t *testing.T, event Event, factID fact.FactID, messageID, upstreamOutput, diagnostic, metadata, nestedMetadata, message string) {
	t.Helper()

	if event.FactRefs[0].FactID != factID {
		t.Fatalf("%s; fact ref = %q, want %q", message, event.FactRefs[0].FactID, factID)
	}
	if event.SourceRefs[0].Message.MessageID != messageID {
		t.Fatalf("%s; source ref message id = %q, want %q", message, event.SourceRefs[0].Message.MessageID, messageID)
	}
	if event.Signature.UpstreamViewRefs[0].OutputSignature != upstreamOutput {
		t.Fatalf("%s; upstream output = %q, want %q", message, event.Signature.UpstreamViewRefs[0].OutputSignature, upstreamOutput)
	}
	if event.Signature.DiagnosticSignatures["projector"] != diagnostic {
		t.Fatalf("%s; diagnostic signature = %q, want %q", message, event.Signature.DiagnosticSignatures["projector"], diagnostic)
	}
	if event.Metadata["k"] != metadata {
		t.Fatalf("%s; metadata = %#v, want %q", message, event.Metadata["k"], metadata)
	}
	assertNestedMetadata(t, event.Metadata, nestedMetadata, message)
}

type fakeProfileStore struct {
	putIn             ProfileRecord
	putOut            ProfileRecord
	getScope          views.Scope
	getID             ProfileID
	getOut            ProfileRecord
	getOK             bool
	listOpts          ProfileListOptions
	listOut           []ProfileRecord
	deleteScope       views.Scope
	deleteID          ProfileID
	deleteEntityScope views.Scope
}

func (s *fakeProfileStore) Put(_ context.Context, record ProfileRecord) (ProfileRecord, error) {
	s.putIn = record
	record.Slots[0].Value = "mutated-store-input"
	record.Slots[0].FactRefs[0].FactID = "mutated-store-input"
	record.FactRefs[0].FactID = "mutated-store-input"
	record.SourceRefs[0].Message.MessageID = "mutated-store-input"
	record.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-store-input"
	record.Signature.DiagnosticSignatures["projector"] = "mutated-store-input"
	record.Metadata["k"] = "mutated-store-input"
	setNestedMetadata(record.Metadata, "mutated-store-input")
	setNestedMetadata(record.Slots[0].Metadata, "mutated-store-input")
	return s.putOut, nil
}

func (s *fakeProfileStore) Get(_ context.Context, scope views.Scope, id ProfileID) (ProfileRecord, bool, error) {
	s.getScope = scope
	s.getID = id
	return s.getOut, s.getOK, nil
}

func (s *fakeProfileStore) List(_ context.Context, opts ProfileListOptions) ([]ProfileRecord, error) {
	s.listOpts = opts
	return s.listOut, nil
}

func (s *fakeProfileStore) Delete(_ context.Context, scope views.Scope, id ProfileID) error {
	s.deleteScope = scope
	s.deleteID = id
	return nil
}

func (s *fakeProfileStore) DeleteEntity(_ context.Context, scope views.Scope) error {
	s.deleteEntityScope = scope
	return nil
}

type fakeTimelineStore struct {
	putIn             Event
	putOut            Event
	getScope          views.Scope
	getID             EventID
	getOut            Event
	getOK             bool
	listOpts          TimelineListOptions
	listOut           []Event
	deleteScope       views.Scope
	deleteID          EventID
	deleteEntityScope views.Scope
}

func (s *fakeTimelineStore) Put(_ context.Context, event Event) (Event, error) {
	s.putIn = event
	event.FactRefs[0].FactID = "mutated-store-input"
	event.SourceRefs[0].Message.MessageID = "mutated-store-input"
	event.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-store-input"
	event.Signature.DiagnosticSignatures["projector"] = "mutated-store-input"
	event.Metadata["k"] = "mutated-store-input"
	setNestedMetadata(event.Metadata, "mutated-store-input")
	*event.OccurredAt = event.OccurredAt.Add(time.Hour)
	*event.ValidFrom = event.ValidFrom.Add(time.Hour)
	return s.putOut, nil
}

func (s *fakeTimelineStore) Get(_ context.Context, scope views.Scope, id EventID) (Event, bool, error) {
	s.getScope = scope
	s.getID = id
	return s.getOut, s.getOK, nil
}

func (s *fakeTimelineStore) List(_ context.Context, opts TimelineListOptions) ([]Event, error) {
	s.listOpts = opts
	return s.listOut, nil
}

func (s *fakeTimelineStore) Delete(_ context.Context, scope views.Scope, id EventID) error {
	s.deleteScope = scope
	s.deleteID = id
	return nil
}

func (s *fakeTimelineStore) DeleteEntity(_ context.Context, scope views.Scope) error {
	s.deleteEntityScope = scope
	return nil
}
