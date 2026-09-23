package retrieval

import (
	"context"
	"strings"
	"testing"

	messagesource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// TestWithSourceQuotesFoldsTheSourceTurnIntoTheFact pins the mechanism the
// measurement pipeline actually runs (deploy.yaml sets source_quotes: 3): a
// fact is a paraphrase, so the turn it came from is folded into its content,
// bounded by SourceQuotes and untouched when the item has no provenance.
func TestWithSourceQuotesFoldsTheSourceTurnIntoTheFact(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := messagesource.NewMessageStore(logStore)
	if err != nil {
		t.Fatal(err)
	}
	records, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conv-1", IdempotencyKey: "turn-1",
		Messages: []coremessage.Message{
			coremessage.NewTextMessage(coremessage.RoleUser, "Melanie read Becoming Nicole last week."),
			coremessage.NewTextMessage(coremessage.RoleAssistant, "She also started Charlotte's Web."),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("appended %d records, want 2", len(records))
	}
	source := func(index int) corememory.SourceRef {
		return corememory.SourceRef{
			Kind: corememory.SourceMessage,
			ID:   records[index].ConversationID + "/" + records[index].ID,
		}
	}
	fact := corememory.ContextItem{
		ID: "fact-1", Kind: corememory.ContextFact, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent("Melanie read a book Caroline recommended."),
		Sources: []corememory.SourceRef{source(0), source(1)},
	}
	raw := corememory.ContextItem{
		ID: "msg-raw", Kind: corememory.ContextRawMessage, SourceClass: corememory.ContextSourceLongTerm,
		Content: coremessage.NewTextContent("Melanie: I read Becoming Nicole."),
		Sources: []corememory.SourceRef{source(0)},
	}

	provider := &Provider{Messages: messages, SourceQuotes: 1}
	enriched := provider.withSourceQuotes(ctx, scope, []corememory.ContextItem{fact, raw})
	if len(enriched) != 2 {
		t.Fatalf("withSourceQuotes returned %d items, want the same 2", len(enriched))
	}
	text := enriched[0].Content.Text()
	if !strings.Contains(text, "Source turn: ") || !strings.Contains(text, "Becoming Nicole") {
		t.Fatalf("fact was not enriched with its source turn: %q", text)
	}
	if strings.Contains(text, "Charlotte's Web") {
		t.Fatalf("SourceQuotes=1 folded more than one source: %q", text)
	}
	if enriched[0].Kind != corememory.ContextFact || enriched[0].ID != "fact-1" {
		t.Fatalf("the fact item changed identity: %#v", enriched[0])
	}
	if got := enriched[1].Content.Text(); got != "Melanie: I read Becoming Nicole." {
		t.Fatalf("a raw message was rewritten: %q", got)
	}

	// Disabled (the library default) must leave content alone.
	plain := (&Provider{Messages: messages}).withSourceQuotes(ctx, scope, []corememory.ContextItem{fact})
	if got := plain[0].Content.Text(); got != "Melanie read a book Caroline recommended." {
		t.Fatalf("SourceQuotes=0 still rewrote the fact: %q", got)
	}
}
