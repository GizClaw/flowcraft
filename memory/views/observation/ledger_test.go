package observation

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestLedgerDescriptorDefaultsAndOptions(t *testing.T) {
	ledger := NewLedger(nil)

	got := ledger.Descriptor()
	if got.ID != DefaultLedgerID {
		t.Fatalf("Descriptor ID = %q, want %q", got.ID, DefaultLedgerID)
	}
	if got.Kind != views.KindObservationLedger {
		t.Fatalf("Descriptor Kind = %q, want %q", got.Kind, views.KindObservationLedger)
	}
	if got.Version != DefaultLedgerVersion {
		t.Fatalf("Descriptor Version = %q, want %q", got.Version, DefaultLedgerVersion)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor Validate() error = %v", err)
	}

	ledger = NewLedger(nil, WithID("project-observations"), WithVersion("v-test"))
	got = ledger.Descriptor()
	want := views.Descriptor{
		ID:      "project-observations",
		Kind:    views.KindObservationLedger,
		Version: "v-test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptor = %#v, want %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("custom Descriptor Validate() error = %v", err)
	}
}

func TestLedgerNilStoreReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	ledger := NewLedger(nil)

	if _, err := ledger.Put(ctx, validObservation("obs-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil store error = %v, want validation", err)
	}
	if _, _, err := ledger.Get(ctx, "obs-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil store error = %v, want validation", err)
	}
	if _, err := ledger.List(ctx, ListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil store error = %v, want validation", err)
	}
	if err := ledger.Delete(ctx, "obs-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil store error = %v, want validation", err)
	}
	if err := ledger.DeleteScope(ctx, validScope()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteScope nil store error = %v, want validation", err)
	}
}

func TestLedgerPutValidation(t *testing.T) {
	ctx := context.Background()
	ledger := NewLedger(&fakeStore{})

	tests := []struct {
		name   string
		mutate func(*Observation)
	}{
		{
			name: "missing id",
			mutate: func(observation *Observation) {
				observation.ID = ""
			},
		},
		{
			name: "missing scope kind",
			mutate: func(observation *Observation) {
				observation.Scope.Kind = ""
			},
		},
		{
			name: "missing scope id",
			mutate: func(observation *Observation) {
				observation.Scope.ID = ""
			},
		},
		{
			name: "missing subject",
			mutate: func(observation *Observation) {
				observation.Subject = ""
			},
		},
		{
			name: "missing predicate",
			mutate: func(observation *Observation) {
				observation.Predicate = ""
			},
		},
		{
			name: "missing object",
			mutate: func(observation *Observation) {
				observation.Object = ""
			},
		},
		{
			name: "invalid negative confidence",
			mutate: func(observation *Observation) {
				observation.Confidence = -0.1
			},
		},
		{
			name: "invalid high confidence",
			mutate: func(observation *Observation) {
				observation.Confidence = 1.1
			},
		},
		{
			name: "missing source refs",
			mutate: func(observation *Observation) {
				observation.SourceRefs = nil
			},
		},
		{
			name: "invalid source ref",
			mutate: func(observation *Observation) {
				observation.SourceRefs[0].Message.MessageID = ""
			},
		},
		{
			name: "missing source revisions",
			mutate: func(observation *Observation) {
				observation.Signature.SourceRevisions = nil
			},
		},
		{
			name: "upstream refs forbidden",
			mutate: func(observation *Observation) {
				observation.Signature.UpstreamViewRefs = []views.UpstreamViewRef{{
					ViewID:          "summary-dag",
					OutputSignature: "summary:v1",
					RecordKey:       "node-1",
				}}
			},
		},
		{
			name: "signature missing view id",
			mutate: func(observation *Observation) {
				observation.Signature.ViewID = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observation := validObservation("obs-1")
			tt.mutate(&observation)

			if _, err := ledger.Put(ctx, observation); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Put(%s) error = %v, want validation", tt.name, err)
			}
		})
	}
}

func TestLedgerGetListDeleteValidation(t *testing.T) {
	ctx := context.Background()
	ledger := NewLedger(&fakeStore{})

	if _, _, err := ledger.Get(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty id error = %v, want validation", err)
	}
	if err := ledger.Delete(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty id error = %v, want validation", err)
	}
	if _, err := ledger.List(ctx, ListOptions{Scope: &Scope{Kind: "conversation"}}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List invalid scope error = %v, want validation", err)
	}
	if err := ledger.DeleteScope(ctx, Scope{ID: "conversation-1"}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteScope invalid scope error = %v, want validation", err)
	}
}

func TestLedgerDelegatesAndClonesPutGetListDelete(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{
		putOut:  validObservation("obs-1"),
		getOut:  validObservation("obs-2"),
		getOK:   true,
		listOut: []Observation{validObservation("obs-3"), validObservation("obs-4")},
	}
	ledger := NewLedger(store)

	input := validObservation("obs-1")
	put, err := ledger.Put(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if store.putIn.ID != input.ID || store.putIn.Subject != input.Subject {
		t.Fatalf("store Put received %+v, want input observation", store.putIn)
	}
	if input.SourceRefs[0].Message.MessageID != "message-1" {
		t.Fatalf("Put shared SourceRefs with caller; input message id = %q", input.SourceRefs[0].Message.MessageID)
	}
	if input.Signature.SourceRevisions[0].Revision != "1" {
		t.Fatalf("Put shared Signature with caller; input revision = %q", input.Signature.SourceRevisions[0].Revision)
	}
	if input.Metadata["k"] != "v" {
		t.Fatalf("Put shared Metadata with caller; input metadata = %#v", input.Metadata)
	}
	assertNestedMetadata(t, input.Metadata, "v", "Put shared nested Metadata with caller")
	setNestedMetadata(store.putIn.Metadata, "mutated-store-captured-input")
	if input.Metadata["k"] != "v" {
		t.Fatalf("store input shared Metadata with caller; input metadata = %#v", input.Metadata)
	}
	assertNestedMetadata(t, input.Metadata, "v", "store input shared nested Metadata with caller")
	put.SourceRefs[0].Message.MessageID = "mutated-return"
	put.Signature.SourceRevisions[0].Revision = "mutated-return"
	put.Signature.DiagnosticSignatures["extractor"] = "mutated-return"
	put.Metadata["k"] = "mutated-return"
	setNestedMetadata(put.Metadata, "mutated-return")
	if store.putOut.SourceRefs[0].Message.MessageID != "message-1" {
		t.Fatalf("Put return shared SourceRefs with store output")
	}
	if store.putOut.Signature.SourceRevisions[0].Revision != "1" {
		t.Fatalf("Put return shared Signature with store output")
	}
	if store.putOut.Signature.DiagnosticSignatures["extractor"] != "observation:v1" {
		t.Fatalf("Put return shared diagnostic signatures with store output")
	}
	if store.putOut.Metadata["k"] != "v" {
		t.Fatalf("Put return shared Metadata with store output")
	}
	assertNestedMetadata(t, store.putOut.Metadata, "v", "Put return shared nested Metadata with store output")

	got, ok, err := ledger.Get(ctx, "obs-2")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if store.getID != "obs-2" {
		t.Fatalf("store Get id = %q, want obs-2", store.getID)
	}
	got.SourceRefs[0].Message.MessageID = "mutated-get"
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	if store.getOut.SourceRefs[0].Message.MessageID != "message-1" || store.getOut.Metadata["k"] != "v" {
		t.Fatalf("Get result shared mutable state with store output")
	}
	assertNestedMetadata(t, store.getOut.Metadata, "v", "Get result shared nested Metadata with store output")

	scope := validScope()
	listed, err := ledger.List(ctx, ListOptions{
		AfterID: "obs-2",
		Limit:   2,
		Scope:   &scope,
		Subject: "user:123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.listOpts.AfterID != "obs-2" || store.listOpts.Limit != 2 || store.listOpts.Subject != "user:123" {
		t.Fatalf("store List options = %+v, want delegated options", store.listOpts)
	}
	if store.listOpts.Scope == nil || *store.listOpts.Scope != validScope() {
		t.Fatalf("store List scope = %+v, want valid scope", store.listOpts.Scope)
	}
	if scope.Kind != "conversation" {
		t.Fatalf("List shared Scope option with caller; scope kind = %q", scope.Kind)
	}
	listed[0].SourceRefs[0].Message.MessageID = "mutated-list"
	listed[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listed[0].Metadata, "mutated-list")
	if store.listOut[0].SourceRefs[0].Message.MessageID != "message-1" || store.listOut[0].Metadata["k"] != "v" {
		t.Fatalf("List result shared mutable state with store output")
	}
	assertNestedMetadata(t, store.listOut[0].Metadata, "v", "List result shared nested Metadata with store output")

	if err := ledger.Delete(ctx, "obs-3"); err != nil {
		t.Fatal(err)
	}
	if store.deleteID != "obs-3" {
		t.Fatalf("store Delete id = %q, want obs-3", store.deleteID)
	}
	if err := ledger.DeleteScope(ctx, validScope()); err != nil {
		t.Fatal(err)
	}
	if store.deleteScope != validScope() {
		t.Fatalf("store DeleteScope scope = %+v, want %+v", store.deleteScope, validScope())
	}
}

func validScope() Scope {
	return Scope{
		Kind:           "conversation",
		ID:             "conversation-1",
		DatasetID:      "dataset-1",
		ConversationID: "conversation-1",
		EntityID:       "user-123",
	}
}

func validObservation(id string) Observation {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	sourceRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: "conversation-1",
			MessageID:      "message-1",
			Span:           &views.Span{Start: 0, End: 10},
		},
	}
	return Observation{
		ID:         id,
		Scope:      validScope(),
		Subject:    "user:123",
		Predicate:  "likes",
		Object:     "coffee",
		Confidence: 0.8,
		SourceRefs: []views.SourceRef{sourceRef},
		Signature: views.ViewSignature{
			ViewID: DefaultLedgerID,
			SourceRevisions: []views.SourceRevision{{
				Kind:      views.SourceMessage,
				SourceKey: sourceRef.StableKey(),
				Revision:  "1",
			}},
			DiagnosticSignatures: map[string]string{"extractor": "observation:v1"},
		},
		CreatedAt: created,
		UpdatedAt: updated,
		Metadata: map[string]any{
			"k": "v",
			"nested": map[string]any{
				"tag":   "v",
				"items": []any{"v", map[string]any{"inner": "v"}},
			},
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

type fakeStore struct {
	putIn       Observation
	putOut      Observation
	getID       string
	getOut      Observation
	getOK       bool
	listOpts    ListOptions
	listOut     []Observation
	deleteID    string
	deleteScope Scope
}

func (s *fakeStore) Put(_ context.Context, observation Observation) (Observation, error) {
	s.putIn = observation
	observation.SourceRefs[0].Message.MessageID = "mutated-store-input"
	observation.Signature.SourceRevisions[0].Revision = "mutated-store-input"
	observation.Metadata["k"] = "mutated-store-input"
	setNestedMetadata(observation.Metadata, "mutated-store-input")
	return s.putOut, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (Observation, bool, error) {
	s.getID = id
	return s.getOut, s.getOK, nil
}

func (s *fakeStore) List(_ context.Context, opts ListOptions) ([]Observation, error) {
	s.listOpts = cloneListOptions(opts)
	if opts.Scope != nil {
		opts.Scope.Kind = "mutated-store-option"
	}
	return s.listOut, nil
}

func (s *fakeStore) Delete(_ context.Context, id string) error {
	s.deleteID = id
	return nil
}

func (s *fakeStore) DeleteScope(_ context.Context, scope Scope) error {
	s.deleteScope = scope
	return nil
}
