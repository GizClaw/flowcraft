package knowledge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

// TestService_SearchDocuments_VectorWithoutCapabilityReturnsNotAvailable
// pins issue #145. Pre-fix, requesting ModeVector / ModeHybrid against
// a chunk repo that only supports doc-level BM25 surfaced a
// NotAvailable error from deep inside SearchDocs. The architectural
// fix lifts the capability check to the service layer via type
// assertion against the new [knowledge.DocVectorSearcher] interface,
// so the error is clear and discoverable BEFORE any backend call.
func TestService_SearchDocuments_VectorWithoutCapabilityReturnsNotAvailable(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds", "a.md", "alpha beta gamma"); err != nil {
		t.Fatalf("put: %v", err)
	}

	for _, mode := range []knowledge.Mode{knowledge.ModeVector, knowledge.ModeHybrid} {
		_, err := svc.SearchDocuments(ctx, knowledge.Query{
			Scope:     knowledge.ScopeSingleDataset,
			DatasetID: "ds",
			Text:      "alpha",
			Mode:      mode,
			TopK:      5,
		})
		if err == nil {
			t.Fatalf("mode=%q expected NotAvailable; got nil", mode)
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("mode=%q expected NotAvailable, got %v (kind=%v)", mode, err, errors.Unwrap(err))
		}
	}
}

// TestDocVectorSearcher_InterfaceShape pins the capability surface
// declared by #145 — implementers must expose SearchDocsByVector with
// the same query shape as DocLevelSearcher.SearchDocs. Asserts the
// interface compiles and is satisfiable.
func TestDocVectorSearcher_InterfaceShape(t *testing.T) {
	var (
		_ knowledge.DocLevelSearcher  = (*stubDocSearcher)(nil)
		_ knowledge.DocVectorSearcher = (*stubDocSearcher)(nil)
	)
}

type stubDocSearcher struct{}

func (stubDocSearcher) SearchDocs(context.Context, knowledge.ChunkQuery) ([]knowledge.Candidate, error) {
	return nil, nil
}
func (stubDocSearcher) SearchDocsByVector(context.Context, knowledge.ChunkQuery) ([]knowledge.Candidate, error) {
	return nil, nil
}
