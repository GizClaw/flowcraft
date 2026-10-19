package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/profile"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// profileSettings serves the resident block with a bound one test can fill:
// four lines, sixty-four tokens.
const profileSettings = `{
  "storage": {"log": {"driver": "workspace"}, "kv": {"driver": "workspace"}},
  "scopes": [{"runtime_id": "memories", "user_id": "u1"}],
  "fact": {"strategy": "none"},
  "summary": {"disabled": true},
  "profile": {"enabled": true, "max_tokens": 64, "max_entries": 4},
  "interval": "0"
}`

// servedBlock runs one context request and returns the resident block it
// carries, which is the item the profile adds to every answer.
func servedBlock(t *testing.T, assembly *Assembly, query string) (corememory.ContextItem, bool) {
	t.Helper()
	result, err := assembly.Context(context.Background(), corememory.ContextRequest{
		Scope: testScope(), ConversationID: "conv-1", Query: query,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.Kind == corememory.ContextProfile {
			return item, true
		}
	}
	return corememory.ContextItem{}, false
}

// profileAssembly derives the two facts a block is made of: a preference and a
// role, both about the same participant of one conversation.
func profileAssembly(t *testing.T, settings string) *Assembly {
	t.Helper()
	deriver := tupleDeriver{tuples: []factTuple{
		{
			turn: "she told me about the pottery class", text: "Caroline loves pottery.",
			entities: []string{"Caroline"}, at: testNow.Add(-2 * time.Hour),
		},
		{
			turn: "we got talking about her work", text: "Caroline works as a counselor.",
			entities: []string{"Caroline"}, at: testNow.Add(-time.Hour),
		},
	}}
	assembly, _ := newTestAssemblyWith(t, settings, WithDeriver(deriver))
	for index, text := range []string{"she told me about the pottery class", "we got talking about her work"} {
		turn := corememory.Turn{
			Scope: testScope(), ConversationID: "conv-1",
			IdempotencyKey: "run-" + string(rune('1'+index)),
			Messages:       []coremessage.Message{textMessage(coremessage.RoleUser, text)},
		}
		if err := assembly.CommitTurn(context.Background(), turn); err != nil {
			t.Fatal(err)
		}
		if err := assembly.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := assembly.Maintain(context.Background(), testScope()); err != nil {
		t.Fatal(err)
	}
	return assembly
}

// TestProfileBlockRidesAlongAndTakesAnEdit is the O11 acceptance at the
// assembly surface: the pass fills a bounded block, every request carries it
// whether or not its query resembles the lines, a host's edit is in the next
// request, the block refuses a write that would break its bound, and a hidden
// fact takes its line with it -- without the block being rewritten.
func TestProfileBlockRidesAlongAndTakesAnEdit(t *testing.T) {
	ctx := context.Background()
	assembly := profileAssembly(t, profileSettings)
	profiles := assembly.Profiles()
	if profiles == nil {
		t.Fatal("the assembly has no block store")
	}
	block, found, err := profiles.Block(ctx, testScope(), "conv-1")
	if err != nil || !found {
		t.Fatalf("block = %+v found = %t err = %v", block, found, err)
	}
	want := "- Caroline loves pottery.\n- Caroline works as a counselor."
	if block.Text != want {
		t.Fatalf("block text = %q, want %q", block.Text, want)
	}

	// Served on a request that shares no words with it, and served first.
	item, ok := servedBlock(t, assembly, "what is the weather like today")
	if !ok {
		t.Fatal("a request without a matching query carried no block")
	}
	if item.Content.Text() != want {
		t.Fatalf("served block = %q", item.Content.Text())
	}
	if item.SourceClass != corememory.ContextSourceProfile || item.ID != "conv-1" {
		t.Fatalf("served item = %+v", item)
	}

	// The bound is hard: the block the settings allow is what a write is
	// measured against, and a write over it is refused whole.
	if block.Entries > 4 {
		t.Fatalf("block entries = %d", block.Entries)
	}
	oversized := strings.Repeat("Caroline loves the long form of this sentence. ", 6)
	if _, err := profiles.SaveExplicit(ctx, testScope(), "conv-1", []string{oversized}); err == nil {
		t.Fatal("the block accepted a write over its bound")
	}
	if after, _, err := profiles.Block(ctx, testScope(), "conv-1"); err != nil || after.Text != want {
		t.Fatalf("refused write changed the block: %+v, %v", after, err)
	}

	// The host's edit is the next request's first line: the block is read on
	// every request, not composed once.
	if _, err := profiles.SaveExplicit(ctx, testScope(), "conv-1",
		[]string{"Caroline prefers the morning shifts."}); err != nil {
		t.Fatal(err)
	}
	edited, ok := servedBlock(t, assembly, "anything at all")
	if !ok {
		t.Fatal("the edited block was not served")
	}
	if !strings.HasPrefix(edited.Content.Text(), "- Caroline prefers the morning shifts.\n") ||
		!strings.Contains(edited.Content.Text(), "counselor") {
		t.Fatalf("served block after the edit = %q", edited.Content.Text())
	}

	// A hidden fact takes its line out of the served block: the line is the
	// pass's reading of that fact, so the hide reaches it. The host's line and
	// the other line stay.
	facts, err := assembly.Facts().List(ctx, testScope(), "conv-1", factview.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pottery := ""
	for _, fact := range facts {
		if strings.Contains(fact.Text, "pottery") {
			pottery = fact.ID
		}
	}
	if pottery == "" {
		t.Fatalf("facts = %#v", facts)
	}
	address := corememory.ContextAddress{Kind: corememory.ContextFact, ConversationID: "conv-1", ItemID: pottery}
	if _, err := assembly.SoftForget(ctx, testScope(), []corememory.ContextAddress{address}, "asked to forget"); err != nil {
		t.Fatal(err)
	}
	hidden, ok := servedBlock(t, assembly, "what is she into")
	if !ok {
		t.Fatal("the block was not served after a forget")
	}
	if strings.Contains(hidden.Content.Text(), "pottery") {
		t.Fatalf("a forgotten fact's line is still served: %q", hidden.Content.Text())
	}
	if !strings.Contains(hidden.Content.Text(), "counselor") {
		t.Fatalf("the forget took more than its line: %q", hidden.Content.Text())
	}
	// Restoring the fact restores the line: the hide was a filter, not a
	// rewrite of the block.
	if _, err := assembly.RestoreForgotten(ctx, testScope(), []corememory.ContextAddress{address}); err != nil {
		t.Fatal(err)
	}
	restored, ok := servedBlock(t, assembly, "what is she into")
	if !ok || !strings.Contains(restored.Content.Text(), "pottery") {
		t.Fatalf("the restored line is not served: %+v", restored.Content.Text())
	}

	// And what is served stays inside the block's own bound.
	stored, _, err := profiles.Load(ctx, testScope(), "conv-1")
	if err != nil {
		t.Fatal(err)
	}
	tokens := 0
	for _, entry := range append(append([]profile.Entry(nil), stored.Explicit...), stored.Distilled...) {
		tokens += pack.EstimatedTokens(entry.Text)
	}
	if tokens > 64 || len(stored.Explicit)+len(stored.Distilled) > 4 {
		t.Fatalf("stored block = %+v (%d tokens)", stored, tokens)
	}
}

// TestProfileBlockIsOffUnlessItIsConfigured pins the default: a deployment that
// does not name the block serves none of it, whatever the store holds, and a
// pass does not fill what the read path would not serve.
func TestProfileBlockIsOffUnlessItIsConfigured(t *testing.T) {
	ctx := context.Background()
	assembly := profileAssembly(t, supersedeSettings)
	if assembly.Profiles() == nil {
		t.Fatal("the block store is not built when the block is off")
	}
	if block, found, err := assembly.Profiles().Block(ctx, testScope(), "conv-1"); err != nil || found {
		t.Fatalf("block = %+v found = %t err = %v, want none", block, found, err)
	}
	if item, ok := servedBlock(t, assembly, "what is she into"); ok {
		t.Fatalf("a disabled block was served: %+v", item)
	}
}
