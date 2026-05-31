package recall_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	memidx "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// TestAdd_StampsContentHash_AvoidsDuplicateAfterSave pins the fix
// for issue #163. Pre-fix, Memory.Add wrote a doc without
// content_hash metadata; a subsequent Save with the same content
// could not match it via dedupHashes and silently produced a
// second doc. After the fix, the Save's MD5 dedup probe recognises
// the Add-written entry and short-circuits to its ID.
func TestAdd_StampsContentHash_AvoidsDuplicateAfterSave(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	resp := `[{"content":"user lives in Beijing","categories":["profile"]}]`
	m, err := recall.New(idx,
		recall.WithLLM(&stubLLM{resp: resp}),
		recall.WithRequireUserID(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	scope := newScope()

	addedID, err := m.Add(ctx, scope, recall.Entry{Content: "user lives in Beijing", Categories: []string{"profile"}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Verify the stamped doc carries content_hash metadata so a Save
	// can find it.
	ns := recall.NamespaceFor(scope)
	resp1, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp1.Items) != 1 {
		t.Fatalf("expected 1 doc after Add, got %d", len(resp1.Items))
	}
	if _, ok := resp1.Items[0].Metadata["content_hash"].(string); !ok {
		t.Fatalf("Add did not stamp content_hash; metadata=%v", resp1.Items[0].Metadata)
	}

	// A Save extracting the exact same content must dedup against
	// the Add-written entry. The stubLLM returns the canned response
	// regardless of inputs, so any non-empty message body suffices.
	res, err := m.Save(ctx, scope, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "where do I live?"}}},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(res.EntryIDs) != 1 {
		t.Fatalf("expected 1 returned id, got %v", res.EntryIDs)
	}
	if res.EntryIDs[0] != addedID {
		t.Fatalf("Save returned id=%q; want existing %q (dedup miss)", res.EntryIDs[0], addedID)
	}

	// Single doc in the namespace — no duplicate.
	resp2, err := idx.List(ctx, ns, retrieval.ListRequest{PageSize: 10})
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(resp2.Items) != 1 {
		t.Fatalf("dedup failed: %d docs after Add+Save; want 1", len(resp2.Items))
	}
}
