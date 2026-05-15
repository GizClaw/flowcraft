package recall_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
	memidx "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func msgUser(text string) llm.Message {
	return llm.Message{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: text}}}
}

func msgAssistant(text string) llm.Message {
	return llm.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: text}}}
}

// TestMemoryBufferRingEviction exercises the default in-memory
// MessageBuffer directly: appending past the cap must evict the
// oldest entries so Recent always reads the freshest K from the tail.
func TestMemoryBufferRingEviction(t *testing.T) {
	ctx := context.Background()
	buf := recall.NewMemoryBuffer(3)
	scope := newScope()

	for i, txt := range []string{"a", "b", "c", "d", "e"} {
		if err := buf.Append(ctx, scope, []llm.Message{msgUser(txt)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := buf.Recent(ctx, scope, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (ring cap)", len(got))
	}
	want := []string{"c", "d", "e"}
	for i, m := range got {
		if m.Content() != want[i] {
			t.Fatalf("buf[%d]=%q want %q", i, m.Content(), want[i])
		}
	}

	tail, err := buf.Recent(ctx, scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[0].Content() != "d" || tail[1].Content() != "e" {
		t.Fatalf("tail=%+v", tail)
	}
}

// TestMemoryBufferIsolatesScopes verifies that two scopes do not see
// each other's messages (canonical scope key namespacing).
func TestMemoryBufferIsolatesScopes(t *testing.T) {
	ctx := context.Background()
	buf := recall.NewMemoryBuffer(8)

	scopeA := recall.Scope{RuntimeID: "rt1", UserID: "alice"}
	scopeB := recall.Scope{RuntimeID: "rt1", UserID: "bob"}

	_ = buf.Append(ctx, scopeA, []llm.Message{msgUser("alice-1"), msgUser("alice-2")})
	_ = buf.Append(ctx, scopeB, []llm.Message{msgUser("bob-1")})

	a, _ := buf.Recent(ctx, scopeA, 10)
	if len(a) != 2 || a[0].Content() != "alice-1" {
		t.Fatalf("scopeA leaked or missing entries: %+v", a)
	}
	b, _ := buf.Recent(ctx, scopeB, 10)
	if len(b) != 1 || b[0].Content() != "bob-1" {
		t.Fatalf("scopeB leaked or missing entries: %+v", b)
	}
}

// TestSaveInjectsRecentTurns wires WithRecentTurns into a real Memory
// and asserts that:
//  1. the first Save sees no recent turns (buffer is empty);
//  2. the extractor's RecentMessages on the SECOND Save contains the
//     messages of the FIRST Save, in chronological order;
//  3. RecentMessages never includes the current batch (which lives in
//     the CONVERSATION slot instead).
func TestSaveInjectsRecentTurns(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "User mentioned a gym", Categories: []string{"events"}}},
			{{Content: "User goes to the gym on weekends", Categories: []string{"events"}}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithRecentTurns(5, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()

	if _, err := m.Save(ctx, scope, []llm.Message{
		msgUser("I joined a new gym yesterday."),
		msgAssistant("Nice — which one?"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Save(ctx, scope, []llm.Message{
		msgUser("I go there every Saturday morning."),
	}); err != nil {
		t.Fatal(err)
	}

	if ex.calls != 2 {
		t.Fatalf("extractor calls=%d want 2", ex.calls)
	}
	if len(ex.gotRecent[0]) != 0 {
		t.Fatalf("first save should see empty RecentMessages, got %+v", ex.gotRecent[0])
	}
	if len(ex.gotRecent[1]) != 2 {
		t.Fatalf("second save should see 2 recent messages, got %d: %+v", len(ex.gotRecent[1]), ex.gotRecent[1])
	}
	if !strings.Contains(ex.gotRecent[1][0].Content(), "joined a new gym") {
		t.Fatalf("oldest recent message wrong: %q", ex.gotRecent[1][0].Content())
	}
	if !strings.Contains(ex.gotRecent[1][1].Content(), "which one") {
		t.Fatalf("newest recent message wrong: %q", ex.gotRecent[1][1].Content())
	}
	// Current batch must NOT bleed into RecentMessages.
	for _, msg := range ex.gotRecent[1] {
		if strings.Contains(msg.Content(), "every Saturday") {
			t.Fatalf("current batch leaked into RecentMessages: %q", msg.Content())
		}
	}
}

// TestSaveDoesNotInjectRecentTurnsWhenDisabled ensures the option is
// opt-in: a Memory built WITHOUT WithRecentTurns must hand the
// extractor an empty RecentMessages slice on every call.
func TestSaveDoesNotInjectRecentTurnsWhenDisabled(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "x"}},
			{{Content: "y"}},
		},
	}
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	for i := 0; i < 2; i++ {
		if _, err := m.Save(ctx, scope, []llm.Message{msgUser("hello")}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	for i, r := range ex.gotRecent {
		if len(r) != 0 {
			t.Fatalf("save %d unexpectedly received recent turns: %+v", i, r)
		}
	}
}

// TestSaveAppendsBufferAfterExtraction guards the ordering contract:
// the current batch must be appended AFTER extraction so a Save that
// fails mid-extract does not poison the next Save's context.
func TestSaveAppendsBufferAfterExtraction(t *testing.T) {
	ctx := context.Background()
	idx := memidx.New()
	ex := &scriptedExtractor{
		facts: [][]recall.ExtractedFact{
			{{Content: "first batch fact"}},
			{{Content: "second batch fact"}},
		},
	}
	buf := recall.NewMemoryBuffer(16)
	m, err := recall.New(idx,
		recall.WithRequireUserID(),
		recall.WithExtractor(ex),
		recall.WithMessageBuffer(buf, 10),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	scope := newScope()
	if _, err := m.Save(ctx, scope, []llm.Message{msgUser("batch-1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(ctx, scope, []llm.Message{msgUser("batch-2")}); err != nil {
		t.Fatal(err)
	}

	stored, _ := buf.Recent(ctx, scope, 10)
	if len(stored) != 2 || stored[0].Content() != "batch-1" || stored[1].Content() != "batch-2" {
		t.Fatalf("buffer state after two saves wrong: %+v", stored)
	}
}
