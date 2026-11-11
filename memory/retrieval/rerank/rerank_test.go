package rerank

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type fakeRuntime struct {
	reply    string
	requests []inference.GenerateRequest
}

func (runtime *fakeRuntime) Generate(_ context.Context, _ model.ModelRef, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	runtime.requests = append(runtime.requests, request)
	return inference.GenerateResponse{Message: coremessage.NewTextMessage(coremessage.RoleAssistant, runtime.reply)}, nil
}

func item(id, text string) corememory.ContextItem {
	return corememory.ContextItem{
		ID: id, Address: corememory.ContextAddress{Kind: corememory.ContextFact, ItemID: id},
		Kind: corememory.ContextFact, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent(text),
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "m1"}},
	}
}

func TestRerankReordersAndKeepsEveryItem(t *testing.T) {
	runtime := &fakeRuntime{reply: `{"order":[2,0]}`}
	model, err := New(runtime, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-chat"}})
	if err != nil {
		t.Fatal(err)
	}
	items := []corememory.ContextItem{item("a", "alpha"), item("b", "beta"), item("c", "gamma")}
	got, err := model.RerankItems(context.Background(), "What happened?", items)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "c" || got[1].ID != "a" || got[2].ID != "b" {
		t.Fatalf("order = %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
	if got[0].Score <= got[1].Score || got[1].Score <= got[2].Score || got[0].Score > 1 {
		t.Fatalf("rerank scores must decay with rank: %v", []float64{got[0].Score, got[1].Score, got[2].Score})
	}
	prompt := runtime.requests[0].Input.Content.Text()
	if !strings.Contains(prompt, "C0: alpha") || !strings.Contains(prompt, "What happened?") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestRerankRejectsMalformedAndOutOfRangeOrders(t *testing.T) {
	for _, reply := range []string{`not json`, `{"order":[]}`, `{"order":[3]}`, `{"order":[-1]}`} {
		model, err := New(&fakeRuntime{reply: reply}, model.ModelRef{ID: model.ModelID{Provider: "p", Name: "m"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.RerankItems(context.Background(), "q", []corememory.ContextItem{item("a", "alpha")}); err == nil {
			t.Fatalf("response %q accepted", reply)
		}
	}
	if _, err := New(nil, model.ModelRef{}); err == nil {
		t.Fatal("incomplete reranker accepted")
	}
}

// TestRerankItemsScoresTheWholeSliceMonotonically pins the fix for the tail
// re-interleaving bug: with more items than maxItems the head is reranked while
// the tail keeps its relative order, but every position must score strictly
// below the one before it, otherwise the packer's score sort promotes tail
// items (which still carry their fused score) above the reranked head.
func TestRerankItemsScoresTheWholeSliceMonotonically(t *testing.T) {
	const total = 60
	items := make([]corememory.ContextItem, 0, total)
	for index := 0; index < total; index++ {
		items = append(items, item(fmt.Sprintf("item-%02d", index), fmt.Sprintf("fact %d", index)))
	}
	// Ask for the head in reverse so the reranked order is visibly different
	// from the input order.
	order := make([]string, 0, 40)
	for index := 39; index >= 0; index-- {
		order = append(order, strconv.Itoa(index))
	}
	runtime := &fakeRuntime{reply: `{"order": [` + strings.Join(order, ",") + `]}`}
	model, err := New(runtime, model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "deepseek-flash"}})
	if err != nil {
		t.Fatal(err)
	}
	reranked, err := model.RerankItems(context.Background(), "what happened", items)
	if err != nil {
		t.Fatal(err)
	}
	if len(reranked) != total {
		t.Fatalf("reranked %d items, want %d", len(reranked), total)
	}
	if reranked[0].ID != "item-39" {
		t.Fatalf("head was not reordered: first = %s", reranked[0].ID)
	}
	if reranked[40].ID != "item-40" {
		t.Fatalf("tail order changed: item 40 = %s", reranked[40].ID)
	}
	for index := 1; index < len(reranked); index++ {
		if reranked[index].Score >= reranked[index-1].Score {
			t.Fatalf("scores not strictly decreasing at %d: %.4f then %.4f (%s after %s)",
				index, reranked[index-1].Score, reranked[index].Score,
				reranked[index].ID, reranked[index-1].ID)
		}
	}
}
