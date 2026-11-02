package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	docview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

const documentCeilingSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "fact": {"strategy": "none"},
  "summary": {"disabled": true},
  "chunk": {"max_runes": 64, "overlap_runes": 8},
  "document": {"max_runes": %d},
  "interval": "0"
}`

func documentOf(id, key, text string) corememory.Document {
	return corememory.Document{
		Scope: testScope(), DatasetID: "docs", DocumentID: id, IdempotencyKey: key,
		Content:    coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: text}}},
		Provenance: []corememory.SourceRef{{Kind: corememory.SourceExternal, ID: "uri:report"}},
	}
}

// TestPutDocumentRejectsTextAboveTheDeploymentCeiling pins the write gate: the
// ceiling is checked before the document is cloned, stored or registered, so a
// payload nobody meant to ingest leaves no revision, no scope and no work
// record behind -- and the host is told which limit it hit.
func TestPutDocumentRejectsTextAboveTheDeploymentCeiling(t *testing.T) {
	ctx := context.Background()
	assembly, _ := newTestAssemblyWith(t, fmt.Sprintf(documentCeilingSettings, 64))
	err := assembly.PutDocument(ctx, documentOf("doc-1", "rev-1", strings.Repeat("a", 65)))
	if !corememory.IsKind(err, corememory.KindInvalidRequest) {
		t.Fatalf("oversized put error = %v, want an invalid request", err)
	}
	if _, found, err := assembly.DocumentStore().Get(ctx, testScope(), "docs", "doc-1"); err != nil || found {
		t.Fatalf("the refused document was stored: found=%v err=%v", found, err)
	}
	if err := assembly.PutDocument(ctx, documentOf("doc-1", "rev-1", strings.Repeat("a", 64))); err != nil {
		t.Fatalf("a document at the deployment ceiling was refused: %v", err)
	}
}

// TestDocumentPassStepsOverADocumentItCannotDerive pins what happens to a
// document stored under a ceiling this deployment no longer derives within --
// an older build wrote it, or the limit was lowered since. The pass must report
// it and move on: holding the stream's cursor on a refusal nobody can lift
// would stop every later document of the scope from ever being derived.
func TestDocumentPassStepsOverADocumentItCannotDerive(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	generous := newTestAssemblyOn(t, ws, fmt.Sprintf(documentCeilingSettings, 400))
	if err := generous.PutDocument(ctx, documentOf("doc-legacy", "rev-1", strings.Repeat("a", 300))); err != nil {
		t.Fatal(err)
	}
	tight := newTestAssemblyOn(t, ws, fmt.Sprintf(documentCeilingSettings, 100))
	if err := tight.PutDocument(ctx, documentOf("doc-after", "rev-1", strings.Repeat("b", 50))); err != nil {
		t.Fatal(err)
	}
	err = tight.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("pass error = %v, want the refusal reported", err)
	}
	legacy, err := tight.DocumentViews().List(ctx, testScope(), "docs", "doc-legacy", docview.ListOptions{})
	if err != nil || len(legacy) != 0 {
		t.Fatalf("the refused document published a build: %#v, %v", legacy, err)
	}
	after, err := tight.DocumentViews().List(ctx, testScope(), "docs", "doc-after", docview.ListOptions{})
	if err != nil || len(after) == 0 {
		t.Fatalf("the document behind the refusal was not derived: %#v, %v", after, err)
	}
	stats, err := tight.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Worker.DocumentsRefused != 1 {
		t.Fatalf("refusals = %d, want 1", stats.Worker.DocumentsRefused)
	}
	// The cursor moved past the refusal, so the pass that follows has nothing
	// left to refuse: a retry converges instead of re-reporting forever.
	if err := tight.RunOnce(ctx); err != nil {
		t.Fatalf("second pass error = %v, want none", err)
	}
}
