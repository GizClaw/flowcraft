package recalltest

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// RunObservationStoreSuite verifies the portable ObservationStore contract.
func RunObservationStoreSuite(t *testing.T, newStore func(testing.TB) recall.ObservationStore) {
	t.Helper()
	t.Run("append_get_list_delete", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t)
		scope := conformanceScope()
		obs1 := recall.Observation{
			ID:         "obs-1",
			Scope:      scope,
			Kind:       recall.ObservationKindTurn,
			SourceID:   "src-1",
			Text:       "first",
			ObservedAt: time.Unix(1, 0),
		}
		obs2 := recall.Observation{
			ID:         "obs-2",
			Scope:      scope,
			Kind:       recall.ObservationKindEvidence,
			SourceID:   "src-2",
			Text:       "second",
			ObservedAt: time.Unix(2, 0),
		}
		if err := store.Append(ctx, []recall.Observation{obs1, obs1, obs2}); err != nil {
			t.Fatalf("append: %v", err)
		}
		got, err := store.Get(ctx, scope, "obs-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Text != "first" {
			t.Fatalf("got.Text = %q", got.Text)
		}
		list, err := store.List(ctx, scope, recall.ObservationListQuery{Kinds: []recall.ObservationKind{recall.ObservationKindEvidence}})
		if err != nil {
			t.Fatalf("list kind: %v", err)
		}
		if len(list) != 1 || list[0].ID != "obs-2" {
			t.Fatalf("list kind = %+v", list)
		}
		if err := store.Delete(ctx, scope, []string{"obs-1"}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		list, err = store.List(ctx, scope, recall.ObservationListQuery{})
		if err != nil {
			t.Fatalf("list after delete: %v", err)
		}
		if len(list) != 1 || list[0].ID != "obs-2" {
			t.Fatalf("list after delete = %+v", list)
		}
		n, err := store.DeleteByScope(ctx, scope)
		if err != nil {
			t.Fatalf("delete scope: %v", err)
		}
		if n != 1 {
			t.Fatalf("delete scope count = %d, want 1", n)
		}
	})
	t.Run("append_merges_spans_for_same_observation", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t)
		scope := conformanceScope()
		base := recall.Observation{
			ID:         "obs-span",
			Scope:      scope,
			Kind:       recall.ObservationKindTurn,
			SourceID:   "src-span",
			Text:       "Alice put the ZXQ capsule in the blue box.",
			ObservedAt: time.Unix(1, 0),
			Spans: []recall.ObservationSpan{{
				ID:            "span-full",
				ObservationID: "obs-span",
				SourceID:      "src-span",
				Kind:          recall.ObservationSpanKindText,
				Text:          "Alice put the ZXQ capsule in the blue box.",
				End:           len("Alice put the ZXQ capsule in the blue box."),
			}},
		}
		quote := recall.Observation{
			ID:         "obs-span",
			Scope:      scope,
			Kind:       recall.ObservationKindEvidence,
			SourceID:   "src-span",
			Text:       "ZXQ capsule in the blue box",
			ObservedAt: time.Unix(1, 0),
			Spans: []recall.ObservationSpan{{
				ID:            "span-quote",
				ObservationID: "obs-span",
				SourceID:      "src-span",
				Kind:          recall.ObservationSpanKindQuote,
				Text:          "ZXQ capsule in the blue box",
				End:           len("ZXQ capsule in the blue box"),
			}},
		}
		if err := store.Append(ctx, []recall.Observation{base, quote}); err != nil {
			t.Fatalf("append merge: %v", err)
		}
		got, err := store.Get(ctx, scope, "obs-span")
		if err != nil {
			t.Fatalf("get merged: %v", err)
		}
		if got.Kind != recall.ObservationKindTurn || len(got.Spans) != 2 {
			t.Fatalf("merged observation = %+v, want turn with two spans", got)
		}
		conflict := quote
		conflict.SourceID = "other-source"
		if err := store.Append(ctx, []recall.Observation{conflict}); err == nil {
			t.Fatal("append conflicting source: got nil error")
		}
	})
}

// RunLinkStoreSuite verifies the portable LinkStore contract.
func RunLinkStoreSuite(t *testing.T, newStore func(testing.TB) recall.LinkStore) {
	t.Helper()
	t.Run("append_find_delete", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t)
		scope := conformanceScope()
		link1 := recall.FactLink{
			ID:        "link-1",
			Scope:     scope,
			Type:      recall.LinkSupports,
			From:      recall.GraphNodeRef{Kind: recall.GraphNodeObservation, ID: "obs-1"},
			To:        recall.GraphNodeRef{Kind: recall.GraphNodeAssertion, ID: "fact-1"},
			MergeKey:  "supports:obs-1:fact-1",
			CreatedAt: time.Unix(1, 0),
		}
		linkDupMerge := link1
		linkDupMerge.ID = "link-dup"
		link2 := recall.FactLink{
			ID:        "link-2",
			Scope:     scope,
			Type:      recall.LinkDerivedFrom,
			From:      recall.GraphNodeRef{Kind: recall.GraphNodeAssertion, ID: "fact-2"},
			To:        recall.GraphNodeRef{Kind: recall.GraphNodeObservation, ID: "obs-1"},
			MergeKey:  "derived:fact-2:obs-1",
			CreatedAt: time.Unix(2, 0),
		}
		if err := store.Append(ctx, []recall.FactLink{link1, linkDupMerge, link2}); err != nil {
			t.Fatalf("append: %v", err)
		}
		list, err := store.List(ctx, scope, recall.LinkListQuery{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("list = %+v, want 2 links after merge-key idempotency", list)
		}
		found, err := store.FindByNode(ctx, scope, recall.GraphNodeRef{Kind: recall.GraphNodeObservation, ID: "obs-1"})
		if err != nil {
			t.Fatalf("find node: %v", err)
		}
		if len(found) != 2 {
			t.Fatalf("find node = %+v, want 2", found)
		}
		byMerge, err := store.FindByMergeKey(ctx, scope, link1.MergeKey)
		if err != nil {
			t.Fatalf("find merge key: %v", err)
		}
		if len(byMerge) != 1 || byMerge[0].ID != "link-1" {
			t.Fatalf("find merge key = %+v", byMerge)
		}
		n, err := store.DeleteByNode(ctx, scope, recall.GraphNodeRef{Kind: recall.GraphNodeAssertion, ID: "fact-2"})
		if err != nil {
			t.Fatalf("delete node: %v", err)
		}
		if n != 1 {
			t.Fatalf("delete node count = %d, want 1", n)
		}
		n, err = store.DeleteByScope(ctx, scope)
		if err != nil {
			t.Fatalf("delete scope: %v", err)
		}
		if n != 1 {
			t.Fatalf("delete scope count = %d, want 1", n)
		}
	})
	t.Run("duplicate_link_id_with_different_payload_conflicts", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t)
		scope := conformanceScope()
		link := recall.FactLink{
			ID:        "link-conflict",
			Scope:     scope,
			Type:      recall.LinkSupports,
			From:      recall.GraphNodeRef{Kind: recall.GraphNodeObservation, ID: "obs-1"},
			To:        recall.GraphNodeRef{Kind: recall.GraphNodeAssertion, ID: "fact-1"},
			CreatedAt: time.Unix(1, 0),
		}
		if err := store.Append(ctx, []recall.FactLink{link}); err != nil {
			t.Fatalf("append first link: %v", err)
		}
		conflict := link
		conflict.To.ID = "fact-2"
		if err := store.Append(ctx, []recall.FactLink{conflict}); !errdefs.IsConflict(err) {
			t.Fatalf("duplicate link id error = %v, want conflict", err)
		}
	})
}
