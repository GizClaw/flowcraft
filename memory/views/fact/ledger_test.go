package fact

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
	if got.Kind != views.KindFactLedger {
		t.Fatalf("Descriptor Kind = %q, want %q", got.Kind, views.KindFactLedger)
	}
	if got.Version != DefaultLedgerVersion {
		t.Fatalf("Descriptor Version = %q, want %q", got.Version, DefaultLedgerVersion)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Descriptor Validate() error = %v", err)
	}

	ledger = NewLedger(nil, WithID("project-facts"), WithVersion("v-test"))
	got = ledger.Descriptor()
	want := views.Descriptor{
		ID:      "project-facts",
		Kind:    views.KindFactLedger,
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

	if _, err := ledger.Put(ctx, validFact("fact-1")); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Put nil store error = %v, want validation", err)
	}
	if _, _, err := ledger.Get(ctx, "fact-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get nil store error = %v, want validation", err)
	}
	if _, err := ledger.List(ctx, ListOptions{}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List nil store error = %v, want validation", err)
	}
	if err := ledger.Delete(ctx, "fact-1"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete nil store error = %v, want validation", err)
	}
	if err := ledger.DeleteSubject(ctx, "user:123"); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteSubject nil store error = %v, want validation", err)
	}
}

func TestLedgerPutValidation(t *testing.T) {
	ctx := context.Background()
	ledger := NewLedger(&fakeStore{})

	tests := []struct {
		name   string
		mutate func(*Fact)
	}{
		{
			name: "missing id",
			mutate: func(fact *Fact) {
				fact.ID = ""
			},
		},
		{
			name: "missing subject",
			mutate: func(fact *Fact) {
				fact.Subject = ""
			},
		},
		{
			name: "missing predicate",
			mutate: func(fact *Fact) {
				fact.Predicate = ""
			},
		},
		{
			name: "missing object",
			mutate: func(fact *Fact) {
				fact.Object = ""
			},
		},
		{
			name: "missing observation refs",
			mutate: func(fact *Fact) {
				fact.ObservationRefs = nil
			},
		},
		{
			name: "missing observation id",
			mutate: func(fact *Fact) {
				fact.ObservationRefs[0].ObservationID = ""
			},
		},
		{
			name: "partial observation scope",
			mutate: func(fact *Fact) {
				fact.ObservationRefs[0].ScopeID = ""
			},
		},
		{
			name: "missing signature",
			mutate: func(fact *Fact) {
				fact.Signature = views.ViewSignature{}
			},
		},
		{
			name: "missing upstream refs",
			mutate: func(fact *Fact) {
				fact.Signature.UpstreamViewRefs = nil
			},
		},
		{
			name: "upstream ref missing view id",
			mutate: func(fact *Fact) {
				fact.Signature.UpstreamViewRefs[0].ViewID = ""
			},
		},
		{
			name: "invalid negative confidence",
			mutate: func(fact *Fact) {
				fact.Confidence = -0.1
			},
		},
		{
			name: "invalid high confidence",
			mutate: func(fact *Fact) {
				fact.Confidence = 1.1
			},
		},
		{
			name: "invalid validity range",
			mutate: func(fact *Fact) {
				from := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
				until := from.Add(-time.Minute)
				fact.ValidFrom = &from
				fact.ValidUntil = &until
			},
		},
		{
			name: "invalid source ref",
			mutate: func(fact *Fact) {
				fact.SourceRefs[0].Message.MessageID = ""
			},
		},
		{
			name: "invalid status",
			mutate: func(fact *Fact) {
				fact.Status = FactStatus("unknown")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fact := validFact("fact-1")
			tt.mutate(&fact)

			if _, err := ledger.Put(ctx, fact); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Put(%s) error = %v, want validation", tt.name, err)
			}
		})
	}
}

func TestLedgerGetListDeleteValidation(t *testing.T) {
	ctx := context.Background()
	ledger := NewLedger(&fakeStore{})
	invalidStatus := FactStatus("unknown")

	if _, _, err := ledger.Get(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Get empty id error = %v, want validation", err)
	}
	if err := ledger.Delete(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Delete empty id error = %v, want validation", err)
	}
	if err := ledger.DeleteSubject(ctx, ""); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("DeleteSubject empty subject error = %v, want validation", err)
	}
	if _, err := ledger.List(ctx, ListOptions{Status: &invalidStatus}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("List invalid status error = %v, want validation", err)
	}
}

func TestLedgerDelegatesNormalizesStatusAndClonesBoundaries(t *testing.T) {
	ctx := context.Background()
	active := FactActive
	store := &fakeStore{
		putOut:  validFact("fact-1"),
		getOut:  validFact("fact-2"),
		getOK:   true,
		listOut: []Fact{validFact("fact-3"), validFact("fact-4")},
	}
	ledger := NewLedger(store)

	input := validFact("fact-1")
	input.Status = ""
	put, err := ledger.Put(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if store.putIn.ID != input.ID || store.putIn.Status != FactActive {
		t.Fatalf("store Put received %+v, want normalized active fact", store.putIn)
	}
	if input.Status != "" {
		t.Fatalf("Put mutated caller status = %q, want empty", input.Status)
	}
	assertFactMutableState(t, input, "message-1", "observation-output:v1", "fact:v1", "v", "v", "Put shared mutable state with caller")
	setNestedMetadata(store.putIn.Metadata, "mutated-store-captured-input")
	if input.Metadata["k"] != "v" {
		t.Fatalf("store input shared Metadata with caller; input metadata = %#v", input.Metadata)
	}
	assertNestedMetadata(t, input.Metadata, "v", "store input shared nested Metadata with caller")

	put.SourceRefs[0].Message.MessageID = "mutated-return"
	put.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-return"
	put.Signature.DiagnosticSignatures["reconciler"] = "mutated-return"
	put.Metadata["k"] = "mutated-return"
	setNestedMetadata(put.Metadata, "mutated-return")
	*put.ValidFrom = put.ValidFrom.Add(time.Hour)
	if store.putOut.SourceRefs[0].Message.MessageID != "message-1" {
		t.Fatalf("Put return shared SourceRefs with store output")
	}
	assertFactMutableState(t, store.putOut, "message-1", "observation-output:v1", "fact:v1", "v", "v", "Put return shared mutable state with store output")

	got, ok, err := ledger.Get(ctx, "fact-2")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if store.getID != "fact-2" {
		t.Fatalf("store Get id = %q, want fact-2", store.getID)
	}
	got.SourceRefs[0].Message.MessageID = "mutated-get"
	got.Metadata["k"] = "mutated-get"
	setNestedMetadata(got.Metadata, "mutated-get")
	assertFactMutableState(t, store.getOut, "message-1", "observation-output:v1", "fact:v1", "v", "v", "Get result shared mutable state with store output")

	listed, err := ledger.List(ctx, ListOptions{
		AfterID:   "fact-2",
		Limit:     2,
		Subject:   "user:123",
		Predicate: "likes",
		Status:    &active,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.listOpts.AfterID != "fact-2" || store.listOpts.Limit != 2 || store.listOpts.Subject != "user:123" || store.listOpts.Predicate != "likes" {
		t.Fatalf("store List options = %+v, want delegated options", store.listOpts)
	}
	if store.listOpts.Status == nil || *store.listOpts.Status != FactActive {
		t.Fatalf("store List status = %+v, want active", store.listOpts.Status)
	}
	if active != FactActive {
		t.Fatalf("List shared Status option with caller; status = %q", active)
	}
	listed[0].SourceRefs[0].Message.MessageID = "mutated-list"
	listed[0].Metadata["k"] = "mutated-list"
	setNestedMetadata(listed[0].Metadata, "mutated-list")
	assertFactMutableState(t, store.listOut[0], "message-1", "observation-output:v1", "fact:v1", "v", "v", "List result shared mutable state with store output")

	if err := ledger.Delete(ctx, "fact-3"); err != nil {
		t.Fatal(err)
	}
	if store.deleteID != "fact-3" {
		t.Fatalf("store Delete id = %q, want fact-3", store.deleteID)
	}
	if err := ledger.DeleteSubject(ctx, "user:123"); err != nil {
		t.Fatal(err)
	}
	if store.deleteSubject != "user:123" {
		t.Fatalf("store DeleteSubject subject = %q, want user:123", store.deleteSubject)
	}
}

func validFact(id FactID) Fact {
	created := time.Date(2026, 6, 9, 1, 2, 3, 0, time.UTC)
	updated := time.Date(2026, 6, 9, 4, 5, 6, 0, time.UTC)
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	validUntil := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	sourceRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: "conversation-1",
			MessageID:      "message-1",
			Span:           &views.Span{Start: 0, End: 10},
		},
	}
	return Fact{
		ID:         id,
		Subject:    "user:123",
		Predicate:  "likes",
		Object:     "coffee",
		Status:     FactActive,
		Confidence: 0.8,
		ValidFrom:  &validFrom,
		ValidUntil: &validUntil,
		ObservationRefs: []ObservationRef{{
			ObservationID: "obs-1",
			ScopeKind:     "conversation",
			ScopeID:       "conversation-1",
		}},
		SourceRefs: []views.SourceRef{sourceRef},
		Signature: views.ViewSignature{
			ViewID: DefaultLedgerID,
			UpstreamViewRefs: []views.UpstreamViewRef{{
				ViewID:          views.ID("observation-ledger"),
				OutputSignature: "observation-output:v1",
				RecordKey:       "obs-1",
			}},
			DiagnosticSignatures: map[string]string{"reconciler": "fact:v1"},
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

func assertFactMutableState(t *testing.T, fact Fact, messageID, upstreamOutput, diagnostic, metadata, nestedMetadata, message string) {
	t.Helper()

	if fact.SourceRefs[0].Message.MessageID != messageID {
		t.Fatalf("%s; source ref message id = %q, want %q", message, fact.SourceRefs[0].Message.MessageID, messageID)
	}
	if fact.Signature.UpstreamViewRefs[0].OutputSignature != upstreamOutput {
		t.Fatalf("%s; upstream output = %q, want %q", message, fact.Signature.UpstreamViewRefs[0].OutputSignature, upstreamOutput)
	}
	if fact.Signature.DiagnosticSignatures["reconciler"] != diagnostic {
		t.Fatalf("%s; diagnostic signature = %q, want %q", message, fact.Signature.DiagnosticSignatures["reconciler"], diagnostic)
	}
	if fact.Metadata["k"] != metadata {
		t.Fatalf("%s; metadata = %#v, want %q", message, fact.Metadata["k"], metadata)
	}
	assertNestedMetadata(t, fact.Metadata, nestedMetadata, message)
}

type fakeStore struct {
	putIn         Fact
	putOut        Fact
	getID         FactID
	getOut        Fact
	getOK         bool
	listOpts      ListOptions
	listOut       []Fact
	deleteID      FactID
	deleteSubject string
}

func (s *fakeStore) Put(_ context.Context, fact Fact) (Fact, error) {
	s.putIn = fact
	fact.SourceRefs[0].Message.MessageID = "mutated-store-input"
	fact.Signature.UpstreamViewRefs[0].OutputSignature = "mutated-store-input"
	fact.Signature.DiagnosticSignatures["reconciler"] = "mutated-store-input"
	fact.Metadata["k"] = "mutated-store-input"
	setNestedMetadata(fact.Metadata, "mutated-store-input")
	*fact.ValidFrom = fact.ValidFrom.Add(time.Hour)
	return s.putOut, nil
}

func (s *fakeStore) Get(_ context.Context, id FactID) (Fact, bool, error) {
	s.getID = id
	return s.getOut, s.getOK, nil
}

func (s *fakeStore) List(_ context.Context, opts ListOptions) ([]Fact, error) {
	s.listOpts = cloneListOptions(opts)
	if opts.Status != nil {
		*opts.Status = FactRetracted
	}
	return s.listOut, nil
}

func (s *fakeStore) Delete(_ context.Context, id FactID) error {
	s.deleteID = id
	return nil
}

func (s *fakeStore) DeleteSubject(_ context.Context, subject string) error {
	s.deleteSubject = subject
	return nil
}
