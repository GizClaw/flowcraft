package views

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSourceRefValidate(t *testing.T) {
	validSpan := &Span{Start: 1, End: 3}

	tests := []struct {
		name string
		ref  SourceRef
		want bool
	}{
		{
			name: "valid message ref",
			ref: SourceRef{
				Kind: SourceMessage,
				Message: &MessageSourceRef{
					ConversationID: "conv-1",
					MessageID:      "msg-1",
					Span:           validSpan,
				},
			},
			want: true,
		},
		{
			name: "valid document ref with grounding metadata",
			ref: SourceRef{
				Kind: SourceDocument,
				Document: &DocumentSourceRef{
					DatasetID:   "dataset-1",
					DocumentID:  "doc-1",
					Version:     "1",
					ContentHash: "sha256:abc",
					Span:        validSpan,
				},
			},
			want: true,
		},
		{
			name: "missing message conversation id",
			ref: SourceRef{
				Kind:    SourceMessage,
				Message: &MessageSourceRef{MessageID: "msg-1"},
			},
		},
		{
			name: "missing document id",
			ref: SourceRef{
				Kind:     SourceDocument,
				Document: &DocumentSourceRef{DatasetID: "dataset-1"},
			},
		},
		{
			name: "wrong kind payload mismatch",
			ref: SourceRef{
				Kind:     SourceMessage,
				Document: &DocumentSourceRef{DatasetID: "dataset-1", DocumentID: "doc-1"},
			},
		},
		{
			name: "multiple payloads",
			ref: SourceRef{
				Kind:     SourceMessage,
				Message:  &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1"},
				Document: &DocumentSourceRef{DatasetID: "dataset-1", DocumentID: "doc-1"},
			},
		},
		{
			name: "missing payload",
			ref:  SourceRef{Kind: SourceMessage},
		},
		{
			name: "invalid source kind",
			ref: SourceRef{
				Kind:    SourceKind("unknown"),
				Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1"},
			},
		},
		{
			name: "negative span start",
			ref: SourceRef{
				Kind:    SourceMessage,
				Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1", Span: &Span{Start: -1, End: 1}},
			},
		},
		{
			name: "span end before start",
			ref: SourceRef{
				Kind:    SourceMessage,
				Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1", Span: &Span{Start: 2, End: 1}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ref.Validate()
			if tt.want && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if !tt.want && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func TestSourceRefStableKey(t *testing.T) {
	message := SourceRef{
		Kind:    SourceMessage,
		Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1", Span: &Span{Start: 1, End: 3}},
	}
	sameMessage := SourceRef{
		Kind:    SourceMessage,
		Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1", Span: &Span{Start: 1, End: 3}},
	}
	differentMessage := SourceRef{
		Kind:    SourceMessage,
		Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-2", Span: &Span{Start: 1, End: 3}},
	}
	differentMessageSpan := SourceRef{
		Kind:    SourceMessage,
		Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1", Span: &Span{Start: 2, End: 3}},
	}

	if message.StableKey() != sameMessage.StableKey() {
		t.Fatalf("equivalent message StableKey() mismatch:\n%s\n%s", message.StableKey(), sameMessage.StableKey())
	}
	if message.StableKey() == differentMessage.StableKey() {
		t.Fatal("StableKey() did not include message id")
	}
	if message.StableKey() == differentMessageSpan.StableKey() {
		t.Fatal("StableKey() did not include message span")
	}

	const wantMessageStableKey = `{"schema":"views.source_ref.v1","kind":"message","message":{"conversation_id":"conv-1","message_id":"msg-1","span":{"start":1,"end":3}}}`
	if got := message.StableKey(); got != wantMessageStableKey {
		t.Fatalf("StableKey() = %s, want golden %s", got, wantMessageStableKey)
	}

	document := SourceRef{
		Kind: SourceDocument,
		Document: &DocumentSourceRef{
			DatasetID:   "dataset-1",
			DocumentID:  "doc-1",
			Version:     "1",
			ContentHash: "sha256:abc",
			Span:        &Span{Start: 4, End: 9},
		},
	}
	sameDocument := SourceRef{
		Kind: SourceDocument,
		Document: &DocumentSourceRef{
			DatasetID:   "dataset-1",
			DocumentID:  "doc-1",
			Version:     "2",
			ContentHash: "sha256:def",
			Span:        &Span{Start: 4, End: 9},
		},
	}
	differentDocumentSpan := document
	differentDocumentSpan.Document = &DocumentSourceRef{DatasetID: "dataset-1", DocumentID: "doc-1", Span: &Span{Start: 5, End: 9}}

	if document.StableKey() != sameDocument.StableKey() {
		t.Fatal("StableKey() included document version or content hash")
	}
	if document.StableKey() == differentDocumentSpan.StableKey() {
		t.Fatal("StableKey() did not include document span")
	}

	const wantDocumentStableKey = `{"schema":"views.source_ref.v1","kind":"document","document":{"dataset_id":"dataset-1","document_id":"doc-1","span":{"start":4,"end":9}}}`
	if got := document.StableKey(); got != wantDocumentStableKey {
		t.Fatalf("StableKey() = %s, want golden %s", got, wantDocumentStableKey)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(document.StableKey()), &decoded); err != nil {
		t.Fatalf("StableKey() JSON roundtrip error = %v", err)
	}
	docPayload, ok := decoded["document"].(map[string]any)
	if !ok {
		t.Fatalf("StableKey() document payload = %T, want object", decoded["document"])
	}
	if _, ok := docPayload["content_hash"]; ok {
		t.Fatalf("StableKey() leaked content hash in payload = %v", docPayload)
	}
	if _, ok := docPayload["version"]; ok {
		t.Fatalf("StableKey() leaked version in payload = %v", docPayload)
	}

	if _, err := (SourceRef{Kind: SourceMessage}).StableKeyE(); err == nil {
		t.Fatal("StableKeyE(invalid) error = nil, want validation error")
	}
}

func TestSourceRefStableKeyPanicsForInvalidRef(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("StableKey(invalid) did not panic")
		}
	}()

	_ = (SourceRef{Kind: SourceMessage}).StableKey()
}

func TestSourceRevisionValidate(t *testing.T) {
	valid := SourceRevision{Kind: SourceMessage, SourceKey: "message-key", Revision: "1"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}
	onlyContentHash := SourceRevision{Kind: SourceMessage, SourceKey: "message-key", ContentHash: "sha256:abc"}
	if err := onlyContentHash.Validate(); err != nil {
		t.Fatalf("Validate(only content hash) error = %v", err)
	}

	tests := []SourceRevision{
		{Kind: SourceKind("invalid"), SourceKey: "message-key", Revision: "1"},
		{Kind: SourceMessage, Revision: "1"},
		{Kind: SourceMessage, SourceKey: "message-key"},
	}
	for _, rev := range tests {
		if err := rev.Validate(); err == nil {
			t.Fatalf("Validate(%+v) error = nil, want error", rev)
		}
	}

	documentKey := SourceRef{
		Kind:     SourceDocument,
		Document: &DocumentSourceRef{DatasetID: "dataset-1", DocumentID: "doc-1"},
	}.StableKey()
	if err := (SourceRevision{Kind: SourceMessage, SourceKey: documentKey, Revision: "1"}).Validate(); err == nil {
		t.Fatal("Validate(message revision with document stable key) error = nil, want error")
	}
	if err := (SourceRevision{Kind: SourceMessage, SourceKey: "opaque-equivalent-key", Revision: "1"}).Validate(); err != nil {
		t.Fatalf("Validate(opaque source key) error = %v", err)
	}
}

func TestDescriptorAndRegistry(t *testing.T) {
	valid := Descriptor{
		ID:      ID("recent"),
		Kind:    KindRecentWindow,
		Version: "v1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate(valid) error = %v", err)
	}

	invalid := []Descriptor{
		{Kind: KindRecentWindow, Version: "v1"},
		{ID: ID("missing-kind"), Version: "v1"},
		{ID: ID("missing-version"), Kind: KindRecentWindow},
		{ID: ID("invalid-kind"), Kind: Kind("invalid"), Version: "v1"},
	}
	for _, descriptor := range invalid {
		if err := descriptor.Validate(); err == nil {
			t.Fatalf("Descriptor.Validate(%+v) error = nil, want error", descriptor)
		}
	}

	var registry Registry
	if err := registry.RegisterView(valid); err != nil {
		t.Fatalf("RegisterView(valid) error = %v", err)
	}
	if err := registry.RegisterView(valid); err == nil {
		t.Fatal("RegisterView duplicate error = nil, want error")
	}
	if got, ok := registry.View(valid.ID); !ok || got != valid {
		t.Fatalf("View(%q) = %+v ok %v, want registered descriptor", valid.ID, got, ok)
	}
	if err := registry.RegisterView(Descriptor{ID: ID("fact"), Kind: KindFactLedger, Version: "v1"}); err != nil {
		t.Fatalf("RegisterView(fact) error = %v", err)
	}
	listed := registry.ListViews()
	if len(listed) != 2 || listed[0].ID != ID("fact") || listed[1].ID != ID("recent") {
		t.Fatalf("ListViews() = %+v, want sorted by ID", listed)
	}
}

func TestUpstreamViewRefMinimalValidationAndDuplicate(t *testing.T) {
	valid := UpstreamViewRef{
		ViewID:          ID("chunks"),
		OutputSignature: "chunks-output:v1",
		RecordKey:       "doc-1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	invalid := []UpstreamViewRef{
		{OutputSignature: "out"},
		{ViewID: ID("chunks")},
	}
	for _, ref := range invalid {
		if err := ref.Validate(); err == nil {
			t.Fatalf("Validate(%+v) error = nil, want error", ref)
		}
	}

	duplicate := ViewSignature{
		ViewID: ID("summary"),
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks"), OutputSignature: "out-a", RecordKey: "doc-1"},
			{ViewID: ID("chunks"), OutputSignature: "out-b", RecordKey: "doc-1"},
		},
	}
	if err := duplicate.Validate(); err == nil {
		t.Fatal("Validate duplicate upstream refs error = nil, want error")
	}

	distinctRecords := ViewSignature{
		ViewID: ID("summary"),
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks"), OutputSignature: "out-a", RecordKey: "doc-1"},
			{ViewID: ID("chunks"), OutputSignature: "out-b", RecordKey: "doc-2"},
		},
	}
	if err := distinctRecords.Validate(); err != nil {
		t.Fatalf("Validate distinct upstream records error = %v", err)
	}
}

func TestViewSignatureLocalValidation(t *testing.T) {
	zero := ViewSignature{}
	if err := zero.Validate(); err != nil {
		t.Fatalf("Validate(zero) error = %v", err)
	}
	if !zero.IsZero() || zero.IsKnown() || zero.HasInputIdentity() {
		t.Fatal("zero signature helpers did not report unknown local identity")
	}

	transformOnly := ViewSignature{ViewID: ID("summary"), TransformSignature: "summary:v1"}
	if err := transformOnly.Validate(); err != nil {
		t.Fatalf("Validate(transform only) error = %v", err)
	}
	if !transformOnly.IsKnown() || transformOnly.HasInputIdentity() {
		t.Fatal("transform-only signature should be known but should not claim input identity")
	}

	if err := (ViewSignature{TransformSignature: "summary:v1"}).Validate(); err == nil {
		t.Fatal("Validate(non-zero missing ViewID) error = nil, want error")
	}

	sourceRef := SourceRef{
		Kind:    SourceMessage,
		Message: &MessageSourceRef{ConversationID: "conv-1", MessageID: "msg-1"},
	}
	withInputs := ViewSignature{
		ViewID: ID("summary"),
		SourceRevisions: []SourceRevision{{
			Kind:      SourceMessage,
			SourceKey: sourceRef.StableKey(),
			Revision:  "1",
		}},
		UpstreamViewRefs: []UpstreamViewRef{{
			ViewID:          ID("window"),
			OutputSignature: "window-output:v1",
		}},
	}
	if err := withInputs.Validate(); err != nil {
		t.Fatalf("Validate(with inputs) error = %v", err)
	}
	if !withInputs.HasInputIdentity() {
		t.Fatal("HasInputIdentity(with inputs) = false, want true")
	}

	duplicateSource := ViewSignature{
		ViewID: ID("summary"),
		SourceRevisions: []SourceRevision{
			{Kind: SourceMessage, SourceKey: "msg-1", Revision: "1"},
			{Kind: SourceMessage, SourceKey: "msg-1", Revision: "2"},
		},
	}
	if err := duplicateSource.Validate(); err == nil {
		t.Fatal("Validate duplicate source revisions error = nil, want error")
	}
}

func TestViewSignatureStaleEqualityIsLocal(t *testing.T) {
	observed := time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC)
	actual := ViewSignature{
		ViewID: ID("summary"),
		SourceRevisions: []SourceRevision{
			{Kind: SourceDocument, SourceKey: "doc-b", Revision: "2", ContentHash: "hash-b", ObservedAt: observed},
			{Kind: SourceDocument, SourceKey: "doc-a", Revision: "1", ContentHash: "hash-a", ObservedAt: observed},
		},
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks-b"), OutputSignature: "chunks-output:doc-b:v1", RecordKey: "doc-b"},
			{ViewID: ID("chunks-a"), OutputSignature: "chunks-output:doc-a:v1", RecordKey: "doc-a"},
		},
		TransformSignature: "summary:v1",
		DiagnosticSignatures: map[string]string{
			"prompt": "prompt:v1",
		},
	}
	want := ViewSignature{
		ViewID: ID("summary"),
		SourceRevisions: []SourceRevision{
			{Kind: SourceDocument, SourceKey: "doc-a", Revision: "1", ContentHash: "hash-a", ObservedAt: observed.Add(time.Hour)},
			{Kind: SourceDocument, SourceKey: "doc-b", Revision: "2", ContentHash: "hash-b", ObservedAt: observed.Add(time.Hour)},
		},
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks-a"), OutputSignature: "chunks-output:doc-a:v1", RecordKey: "doc-a"},
			{ViewID: ID("chunks-b"), OutputSignature: "chunks-output:doc-b:v1", RecordKey: "doc-b"},
		},
		TransformSignature: "summary:v1",
		DiagnosticSignatures: map[string]string{
			"prompt": "prompt:v2",
		},
	}

	if err := actual.Validate(); err != nil {
		t.Fatalf("Validate(actual) error = %v", err)
	}
	if actual.IsStaleAgainst(want) {
		t.Fatal("IsStaleAgainst equivalent local signature = true, want false")
	}
	if got := actual.StaleCountAgainst(want); got != 0 {
		t.Fatalf("StaleCountAgainst equivalent local signature = %d, want 0", got)
	}

	mutatedTransform := cloneViewSignature(want)
	mutatedTransform.TransformSignature = "summary:v2"
	if !actual.IsStaleAgainst(mutatedTransform) {
		t.Fatal("IsStaleAgainst transform mismatch = false, want true")
	}
	mutatedSource := cloneViewSignature(want)
	mutatedSource.SourceRevisions[0].Revision = "3"
	if !actual.IsStaleAgainst(mutatedSource) {
		t.Fatal("IsStaleAgainst source revision mismatch = false, want true")
	}
	mutatedUpstream := cloneViewSignature(want)
	mutatedUpstream.UpstreamViewRefs[0].OutputSignature = "chunks-output:doc-a:v2"
	if !actual.IsStaleAgainst(mutatedUpstream) {
		t.Fatal("IsStaleAgainst upstream output signature mismatch = false, want true")
	}
	mutatedView := cloneViewSignature(want)
	mutatedView.ViewID = ID("other")
	if !actual.IsStaleAgainst(mutatedView) {
		t.Fatal("IsStaleAgainst view mismatch = false, want true")
	}
}

func TestCloneViewSignatureClonesNestedState(t *testing.T) {
	original := ViewSignature{
		ViewID: ID("summary"),
		SourceRevisions: []SourceRevision{{
			Kind:        SourceDocument,
			SourceKey:   "doc-1",
			Revision:    "1",
			ContentHash: "hash-1",
		}},
		UpstreamViewRefs: []UpstreamViewRef{{
			ViewID:          ID("chunks"),
			OutputSignature: "chunks-output:v1",
			RecordKey:       "doc-1",
		}},
		DiagnosticSignatures: map[string]string{
			"prompt": "p1",
		},
	}

	clone := cloneViewSignature(original)
	clone.SourceRevisions[0].Revision = "2"
	clone.UpstreamViewRefs[0].OutputSignature = "chunks-output:v2"
	clone.DiagnosticSignatures["prompt"] = "p2"

	if original.SourceRevisions[0].Revision != "1" {
		t.Fatalf("cloneViewSignature shared source revisions; original revision = %q", original.SourceRevisions[0].Revision)
	}
	if original.UpstreamViewRefs[0].OutputSignature != "chunks-output:v1" {
		t.Fatalf("cloneViewSignature shared upstream refs; original output signature = %q", original.UpstreamViewRefs[0].OutputSignature)
	}
	if original.DiagnosticSignatures["prompt"] != "p1" {
		t.Fatalf("cloneViewSignature shared diagnostic signatures; original prompt = %q", original.DiagnosticSignatures["prompt"])
	}
}

func TestViewSignatureStaleCountAgainstExactCount(t *testing.T) {
	actual := ViewSignature{
		ViewID: ID("summary"),
		SourceRevisions: []SourceRevision{
			{Kind: SourceDocument, SourceKey: "doc-1", Revision: "1", ContentHash: "hash-1"},
			{Kind: SourceDocument, SourceKey: "doc-2", Revision: "1", ContentHash: "hash-2"},
		},
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks"), OutputSignature: "chunks-output:doc-1:v1", RecordKey: "doc-1"},
			{ViewID: ID("chunks"), OutputSignature: "chunks-output:doc-2:v1", RecordKey: "doc-2"},
		},
		TransformSignature: "summary:v1",
		DiagnosticSignatures: map[string]string{
			"prompt": "p1",
		},
	}

	oneDifference := cloneViewSignature(actual)
	oneDifference.TransformSignature = "summary:v2"
	if got := actual.StaleCountAgainst(oneDifference); got != 1 {
		t.Fatalf("StaleCountAgainst single transform difference = %d, want 1", got)
	}

	componentDifference := cloneViewSignature(actual)
	componentDifference.DiagnosticSignatures["prompt"] = "p2"
	if got := actual.StaleCountAgainst(componentDifference); got != 0 {
		t.Fatalf("StaleCountAgainst diagnostic component difference = %d, want 0", got)
	}

	sourceAdded := cloneViewSignature(actual)
	sourceAdded.SourceRevisions = append(sourceAdded.SourceRevisions, SourceRevision{
		Kind:        SourceDocument,
		SourceKey:   "doc-3",
		Revision:    "1",
		ContentHash: "hash-3",
	})
	if got := actual.StaleCountAgainst(sourceAdded); got != 1 {
		t.Fatalf("StaleCountAgainst source addition = %d, want 1", got)
	}

	upstreamAdded := cloneViewSignature(actual)
	upstreamAdded.UpstreamViewRefs = append(upstreamAdded.UpstreamViewRefs, UpstreamViewRef{
		ViewID:          ID("chunks"),
		OutputSignature: "chunks-output:doc-3:v1",
		RecordKey:       "doc-3",
	})
	if got := actual.StaleCountAgainst(upstreamAdded); got != 1 {
		t.Fatalf("StaleCountAgainst upstream addition = %d, want 1", got)
	}

	manyDifferences := cloneViewSignature(actual)
	manyDifferences.ViewID = ID("summary-v2")
	manyDifferences.SourceRevisions[0].Revision = "2"
	manyDifferences.SourceRevisions[1].Revision = "2"
	manyDifferences.UpstreamViewRefs[0].OutputSignature = "chunks-output:doc-1:v2"
	manyDifferences.UpstreamViewRefs[1].OutputSignature = "chunks-output:doc-2:v2"
	manyDifferences.TransformSignature = "summary:v2"
	manyDifferences.DiagnosticSignatures["prompt"] = "p2"
	if got := actual.StaleCountAgainst(manyDifferences); got != 4 {
		t.Fatalf("StaleCountAgainst multiple dimension differences = %d, want 4", got)
	}
}

func TestSortedSignatureComparisonDoesNotMutateInputs(t *testing.T) {
	actual := ViewSignature{
		ViewID: ID("summary"),
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks-b"), OutputSignature: "out-b", RecordKey: "b"},
			{ViewID: ID("chunks-a"), OutputSignature: "out-a", RecordKey: "a"},
		},
	}
	want := ViewSignature{
		ViewID: ID("summary"),
		UpstreamViewRefs: []UpstreamViewRef{
			{ViewID: ID("chunks-a"), OutputSignature: "out-a", RecordKey: "a"},
			{ViewID: ID("chunks-b"), OutputSignature: "out-b", RecordKey: "b"},
		},
	}

	if actual.IsStaleAgainst(want) {
		t.Fatal("IsStaleAgainst reordered upstream refs = true, want false")
	}
	if !reflect.DeepEqual(actual.UpstreamViewRefs, []UpstreamViewRef{
		{ViewID: ID("chunks-b"), OutputSignature: "out-b", RecordKey: "b"},
		{ViewID: ID("chunks-a"), OutputSignature: "out-a", RecordKey: "a"},
	}) {
		t.Fatalf("IsStaleAgainst mutated upstream order: %+v", actual.UpstreamViewRefs)
	}
}
