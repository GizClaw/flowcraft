package kanban

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Produce / deep copy
// ---------------------------------------------------------------------------

func TestBoard_Produce_ReturnsDeepCopy(t *testing.T) {
	b := NewBoard("scope-dc1")
	defer b.Close()
	card := b.Produce("task", "p", map[string]any{"key": "val"}, WithMeta("m", "v"))

	card.Status = CardDone
	card.Meta["m"] = "mutated"

	internal := b.Last(CardFilter{Type: "task"})
	if internal.Status != CardPending {
		t.Fatalf("internal card should still be Pending, got %s", internal.Status)
	}
	if internal.Meta["m"] != "v" {
		t.Fatalf("internal meta should be 'v', got %s", internal.Meta["m"])
	}
}

func TestBoard_Query_ReturnsDeepCopies(t *testing.T) {
	b := NewBoard("scope-dc2")
	defer b.Close()
	b.Produce("task", "p", nil)
	b.Produce("task", "p", nil)

	cards := b.Query(CardFilter{Type: "task"})
	for _, c := range cards {
		c.Status = CardDone
	}

	internal := b.Query(CardFilter{Type: "task"})
	for _, c := range internal {
		if c.Status != CardPending {
			t.Fatalf("mutation through Query should not affect internal state, got %s", c.Status)
		}
	}
}

func TestBoard_Last_ReturnsDeepCopy(t *testing.T) {
	b := NewBoard("scope-dc3")
	defer b.Close()
	b.Produce("task", "p", "payload")

	last := b.Last(CardFilter{Type: "task"})
	last.Producer = "mutated"

	check := b.Last(CardFilter{Type: "task"})
	if check.Producer == "mutated" {
		t.Fatal("Last() should return a copy; mutation should not affect internal state")
	}
}

func TestBoard_RawCards_ReturnsDeepCopies(t *testing.T) {
	b := NewBoard("scope-dc4")
	defer b.Close()
	b.Produce("task", "p", nil)

	cards := b.RawCards()
	cards[0].Type = "mutated"

	check := b.RawCards()
	if check[0].Type == "mutated" {
		t.Fatal("RawCards() should return copies; mutation should not affect internal state")
	}
}

// ---------------------------------------------------------------------------
// Claim / Done / Fail state transitions
// ---------------------------------------------------------------------------

func TestBoard_DoubleClaimFails(t *testing.T) {
	b := NewBoard("scope-dcf")
	defer b.Close()
	card := b.Produce("task", "p", nil)

	if !b.Claim(card.ID, "a1") {
		t.Fatal("first claim should succeed")
	}
	if b.Claim(card.ID, "a2") {
		t.Fatal("second claim should fail (already claimed)")
	}
}

func TestBoard_DoneOnPendingFails(t *testing.T) {
	b := NewBoard("scope-dop")
	defer b.Close()
	card := b.Produce("task", "p", nil)

	if b.Done(card.ID, "result") {
		t.Fatal("Done on pending card should fail")
	}
}

func TestBoard_DoneAfterDoneFails(t *testing.T) {
	b := NewBoard("scope-dad")
	defer b.Close()
	card := b.Produce("task", "p", nil)
	b.Claim(card.ID, "a")
	b.Done(card.ID, "r1")

	if b.Done(card.ID, "r2") {
		t.Fatal("double Done should fail")
	}
}

func TestBoard_FailOnPendingSucceeds(t *testing.T) {
	b := NewBoard("scope-fop")
	defer b.Close()
	card := b.Produce("task", "p", nil)

	if !b.Fail(card.ID, "error") {
		t.Fatal("Fail on pending card should succeed")
	}
	for _, c := range b.Cards() {
		if c.ID == card.ID {
			if c.Status != string(CardFailed) {
				t.Fatalf("expected CardFailed, got %s", c.Status)
			}
			if c.Error != "error" {
				t.Fatalf("expected error message, got %q", c.Error)
			}
			return
		}
	}
	t.Fatal("card not found")
}

func TestBoard_QueryDoneFailedPendingCounts(t *testing.T) {
	b := NewBoard("scope-qcounts")
	defer b.Close()

	c1 := b.Produce("task", "p1", "payload1")
	b.Claim(c1.ID, "a1")
	b.Done(c1.ID, "result1")

	c2 := b.Produce("task", "p2", "payload2")
	b.Claim(c2.ID, "a2")
	b.Fail(c2.ID, "oops")

	b.Produce("task", "p3", "payload3")

	if len(b.Query(CardFilter{Status: CardDone})) != 1 {
		t.Fatalf("done: %d", len(b.Query(CardFilter{Status: CardDone})))
	}
	if len(b.Query(CardFilter{Status: CardFailed})) != 1 {
		t.Fatalf("failed: %d", len(b.Query(CardFilter{Status: CardFailed})))
	}
	if len(b.Query(CardFilter{Status: CardPending})) != 1 {
		t.Fatalf("pending: %d", len(b.Query(CardFilter{Status: CardPending})))
	}
}

// ---------------------------------------------------------------------------
// GetCardByID
// ---------------------------------------------------------------------------

func TestBoard_GetCardByID(t *testing.T) {
	b := NewBoard("scope-k4")
	defer b.Close()

	card := b.Produce("task", "p", map[string]any{"k": "v"})

	got, ok := b.GetCardByID(card.ID)
	if !ok {
		t.Fatal("expected card to be found")
	}
	if got.ID != card.ID {
		t.Fatalf("expected ID %q, got %q", card.ID, got.ID)
	}

	got.Status = CardDone
	check, _ := b.GetCardByID(card.ID)
	if check.Status != CardPending {
		t.Fatal("GetCardByID should return a deep copy; mutation must not affect internal state")
	}
}

func TestBoard_GetCardByID_NotFound(t *testing.T) {
	b := NewBoard("scope-k4-nf")
	defer b.Close()

	_, ok := b.GetCardByID("nonexistent")
	if ok {
		t.Fatal("expected ok=false for nonexistent card")
	}
}

func TestBoard_GetCardByID_AfterRemap(t *testing.T) {
	b := NewBoard("scope-k4-remap")
	defer b.Close()

	card := b.Produce("task", "p", nil)
	oldID := card.ID
	newID := "remapped-id"
	b.RemapCardID(oldID, newID)

	_, ok := b.GetCardByID(oldID)
	if ok {
		t.Fatal("old ID should no longer be found after remap")
	}

	got, ok := b.GetCardByID(newID)
	if !ok {
		t.Fatal("new ID should be found after remap")
	}
	if got.ID != newID {
		t.Fatalf("expected ID %q, got %q", newID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// CountByStatus
// ---------------------------------------------------------------------------

func TestBoard_CountByStatus(t *testing.T) {
	b := NewBoard("scope-k3")
	defer b.Close()

	c1 := b.Produce("task", "p", nil)
	c2 := b.Produce("task", "p", nil)
	b.Produce("signal", "p", nil)

	if got := b.CountByStatus(CardPending, ""); got != 3 {
		t.Fatalf("all pending: expected 3, got %d", got)
	}
	if got := b.CountByStatus(CardPending, "task"); got != 2 {
		t.Fatalf("pending tasks: expected 2, got %d", got)
	}
	if got := b.CountByStatus(CardPending, "signal"); got != 1 {
		t.Fatalf("pending signals: expected 1, got %d", got)
	}

	b.Claim(c1.ID, "a1")
	if got := b.CountByStatus(CardPending, "task"); got != 1 {
		t.Fatalf("after claim: expected 1 pending task, got %d", got)
	}
	if got := b.CountByStatus(CardClaimed, ""); got != 1 {
		t.Fatalf("after claim: expected 1 claimed, got %d", got)
	}

	b.Done(c1.ID, "result")
	if got := b.CountByStatus(CardDone, ""); got != 1 {
		t.Fatalf("after done: expected 1 done, got %d", got)
	}

	b.Fail(c2.ID, "oops")
	if got := b.CountByStatus(CardFailed, ""); got != 1 {
		t.Fatalf("after fail: expected 1 failed, got %d", got)
	}
	if got := b.CountByStatus(CardPending, ""); got != 1 {
		t.Fatalf("remaining pending: expected 1 (signal), got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Watch / WatchFiltered
// ---------------------------------------------------------------------------

func waitBoardCard(t *testing.T, ch <-chan *Card, desc string) *Card {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", desc)
		return nil
	}
}

func TestBoard_WatcherSeesClaimDoneFail(t *testing.T) {
	b := NewBoard("scope-watcher")
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.WatchFiltered(ctx, CardFilter{Type: "task"})

	card1 := b.Produce("task", "orch", map[string]any{"n": 1})

	recv := waitBoardCard(t, ch, "produce")
	if recv.ID != card1.ID || recv.Status != CardPending {
		t.Fatalf("expected pending card1, got status=%s", recv.Status)
	}

	b.Claim(card1.ID, "agent-1")
	recv = waitBoardCard(t, ch, "claim")
	if recv.Status != CardClaimed {
		t.Fatalf("expected claimed, got %s", recv.Status)
	}
	if recv.Consumer != "agent-1" {
		t.Fatalf("expected agent-1, got %s", recv.Consumer)
	}

	b.Done(card1.ID, "result-data")
	recv = waitBoardCard(t, ch, "done")
	if recv.Status != CardDone {
		t.Fatalf("expected done, got %s", recv.Status)
	}

	card2 := b.Produce("task", "orch", map[string]any{"n": 2})
	recv = waitBoardCard(t, ch, "produce card2")
	if recv.ID != card2.ID {
		t.Fatal("expected card2")
	}

	b.Claim(card2.ID, "agent-2")
	recv = waitBoardCard(t, ch, "claim card2")
	if recv.Status != CardClaimed {
		t.Fatalf("expected claimed, got %s", recv.Status)
	}

	b.Fail(card2.ID, "agent error")
	recv = waitBoardCard(t, ch, "fail")
	if recv.Status != CardFailed {
		t.Fatalf("expected failed, got %s", recv.Status)
	}
	if recv.Error != "agent error" {
		t.Fatalf("expected error 'agent error', got %q", recv.Error)
	}
}

func TestBoard_WatcherReceivesSnapshot(t *testing.T) {
	b := NewBoard("scope-snap")
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.WatchFiltered(ctx, CardFilter{})

	card := b.Produce("task", "p", map[string]any{"v": 1}, WithMeta("key", "val"))

	recv := waitBoardCard(t, ch, "produce")
	if recv.ID != card.ID {
		t.Fatal("wrong card")
	}

	b.Claim(card.ID, "agent")

	if recv.Status != CardPending {
		t.Fatal("received snapshot should remain Pending even after source card was claimed")
	}

	if recv.Meta["key"] != "val" {
		t.Fatal("meta should be copied to snapshot")
	}
}

func TestBoard_MultipleWatchersFiltered(t *testing.T) {
	b := NewBoard("scope-multi-watch")
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskCh := b.WatchFiltered(ctx, CardFilter{Type: "task"})
	resultCh := b.WatchFiltered(ctx, CardFilter{Type: "result"})
	allCh := b.WatchFiltered(ctx, CardFilter{})

	b.Produce("task", "p", nil)
	b.Produce("result", "p", nil)
	b.Produce("signal", "p", nil)

	recv := waitBoardCard(t, taskCh, "task")
	if recv.Type != "task" {
		t.Fatalf("taskCh: expected task, got %s", recv.Type)
	}

	recv = waitBoardCard(t, resultCh, "result")
	if recv.Type != "result" {
		t.Fatalf("resultCh: expected result, got %s", recv.Type)
	}

	received := 0
	timeout := time.After(time.Second)
drain:
	for {
		select {
		case <-allCh:
			received++
			if received == 3 {
				break drain
			}
		case <-timeout:
			break drain
		}
	}
	if received != 3 {
		t.Fatalf("allCh: expected 3 events, got %d", received)
	}
}

func TestBoard_WatcherCleanupOnCancel(t *testing.T) {
	b := NewBoard("scope-cleanup")
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())

	ch := b.WatchFiltered(ctx, CardFilter{})
	cancel()

	time.Sleep(50 * time.Millisecond)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed after context cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel should be closed")
	}

	b.Produce("task", "p", nil)
}

func TestBoard_WatcherChannelFull_DoesNotPanic(t *testing.T) {
	b := NewBoard("scope-k5")
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := b.WatchFiltered(ctx, CardFilter{})

	for i := 0; i < watchBufSize+10; i++ {
		b.Produce("task", "p", nil)
	}

	drained := 0
	for {
		select {
		case <-ch:
			drained++
		default:
			goto done
		}
	}
done:
	if drained != watchBufSize {
		t.Fatalf("expected to receive exactly %d buffered cards, got %d", watchBufSize, drained)
	}
}

func TestBoard_Watch_ClosesOnBoardClose(t *testing.T) {
	b := NewBoard("scope-watch-close")
	ch := b.Watch(context.Background())
	b.Close()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected watch channel to be closed after Board.Close")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watch channel to close")
	}
}

// ---------------------------------------------------------------------------
// Cards / Timeline / Topology (view methods)
// ---------------------------------------------------------------------------

func TestBoard_Cards_FiltersResultCards(t *testing.T) {
	b := NewBoard("scope-filter")
	defer b.Close()

	b.Produce("task", "agent-main", TaskPayload{Query: "q1", TargetAgentID: "t1"})
	b.Produce("result", "agent-1", ResultPayload{Output: "r1"},
		WithConsumer("agent-main"), WithMeta("task_card_id", "c1"))
	b.Produce("task", "agent-main", TaskPayload{Query: "q2", TargetAgentID: "t2"})

	cards := b.Cards()
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (result filtered), got %d", len(cards))
	}
	for _, c := range cards {
		if c.Type == "result" {
			t.Fatal("result card should be filtered from Cards()")
		}
	}
}

func TestBoard_Cards_ExtractsPayloadFields(t *testing.T) {
	b := NewBoard("scope-payload")
	defer b.Close()

	card := b.Produce("task", "orch", TaskPayload{Query: "the query", TargetAgentID: "the_tpl"})
	b.Claim(card.ID, "agent-1")
	b.Done(card.ID, map[string]any{
		"query":           "the query",
		"target_agent_id": "the_tpl",
		"output":          "the output",
	})

	cards := b.Cards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	c := cards[0]
	if c.Query != "the query" {
		t.Fatalf("expected query 'the query', got %q", c.Query)
	}
	if c.TargetAgentID != "the_tpl" {
		t.Fatalf("expected target_agent_id 'the_tpl', got %q", c.TargetAgentID)
	}
	if c.Output != "the output" {
		t.Fatalf("expected output 'the output', got %q", c.Output)
	}
}

func TestBoard_Cards_ExtractsRunID(t *testing.T) {
	b := NewBoard("scope-runid")
	defer b.Close()

	card := b.Produce("task", "orch", TaskPayload{Query: "q1", TargetAgentID: "agent-1"})
	b.Claim(card.ID, "agent-1")
	b.Done(card.ID, map[string]any{
		"query":           "q1",
		"target_agent_id": "agent-1",
		"output":          "done",
		"run_id":          "run-abc",
	})

	cards := b.Cards()
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].RunID != "run-abc" {
		t.Fatalf("expected run_id 'run-abc', got %q", cards[0].RunID)
	}
}

func TestBoard_Timeline_FiltersResultCards(t *testing.T) {
	b := NewBoard("scope-tl-filter")
	defer b.Close()

	b.Produce("task", "orch", TaskPayload{Query: "q1"})
	b.Produce("result", "agent", ResultPayload{Output: "r1"})

	timeline := b.Timeline()
	if len(timeline) != 1 {
		t.Fatalf("expected 1 timeline entry, got %d", len(timeline))
	}
	if timeline[0].Type == "result" {
		t.Fatal("result should not appear in timeline")
	}
}

func TestBoard_Topology(t *testing.T) {
	b := NewBoard("scope-topo")
	defer b.Close()

	b.Produce("task", "agent-main", TaskPayload{Query: "q1"}, WithConsumer("agent-1"))
	b.Produce("task", "agent-main", TaskPayload{Query: "q2"}, WithConsumer("agent-2"))

	topo := b.Topology()
	if len(topo.Nodes) != 3 {
		t.Fatalf("expected 3 nodes (producer + 2 consumers), got %d", len(topo.Nodes))
	}
	if len(topo.Edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(topo.Edges))
	}
}

// ---------------------------------------------------------------------------
// Payload extraction helpers
// ---------------------------------------------------------------------------

func TestExtractPayloadFieldsPublic(t *testing.T) {
	q, tid, o := ExtractPayloadFieldsPublic(TaskPayload{Query: "q", TargetAgentID: "t"})
	if q != "q" || tid != "t" || o != "" {
		t.Fatalf("unexpected: q=%q tid=%q o=%q", q, tid, o)
	}

	q, _, o = ExtractPayloadFieldsPublic(map[string]any{"query": "q2", "output": "o2"})
	if q != "q2" || o != "o2" {
		t.Fatalf("unexpected map: q=%q o=%q", q, o)
	}
}

func TestExtractRunID(t *testing.T) {
	if id := extractRunID(map[string]any{"run_id": "r1"}); id != "r1" {
		t.Fatalf("expected r1, got %q", id)
	}
	if id := extractRunID(map[string]any{"other": "val"}); id != "" {
		t.Fatalf("expected empty, got %q", id)
	}
	if id := extractRunID(TaskPayload{Query: "q"}); id != "" {
		t.Fatalf("expected empty for non-map, got %q", id)
	}
	if id := extractRunID(nil); id != "" {
		t.Fatalf("expected empty for nil, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

func TestRestoreTaskBoard_AllStates(t *testing.T) {
	cards := []*KanbanCardModel{
		{
			ID: "c1", RuntimeID: "s1", Type: "task", Status: "pending",
			Producer: "copilot", Consumer: "*",
			Query: "q1", TargetAgentID: "agent-1",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
		{
			ID: "c2", RuntimeID: "s1", Type: "task", Status: "claimed",
			Producer: "copilot", Consumer: "agent-2",
			Query: "q2", TargetAgentID: "agent-2",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
		{
			ID: "c3", RuntimeID: "s1", Type: "task", Status: "done",
			Producer: "copilot", Consumer: "agent-3",
			Query: "q3", TargetAgentID: "agent-3", Output: "result", RunID: "run-1",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
		{
			ID: "c4", RuntimeID: "s1", Type: "task", Status: "failed",
			Producer: "copilot", Consumer: "agent-4",
			Query: "q4", TargetAgentID: "agent-4", Error: "timeout",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}

	tb := RestoreTaskBoard("s1", cards)
	defer tb.Close()

	if tb.ScopeID() != "s1" {
		t.Fatalf("expected scope s1, got %q", tb.ScopeID())
	}

	infos := tb.Cards()
	if len(infos) != 4 {
		t.Fatalf("expected 4 cards, got %d", len(infos))
	}

	statusMap := make(map[string]string)
	for _, c := range infos {
		statusMap[c.ID] = c.Status
	}

	if statusMap["c1"] != "pending" {
		t.Fatalf("c1: expected pending, got %s", statusMap["c1"])
	}
	if statusMap["c2"] != "claimed" {
		t.Fatalf("c2: expected claimed, got %s", statusMap["c2"])
	}
	if statusMap["c3"] != "done" {
		t.Fatalf("c3: expected done, got %s", statusMap["c3"])
	}
	if statusMap["c4"] != "failed" {
		t.Fatalf("c4: expected failed, got %s", statusMap["c4"])
	}

	for _, c := range infos {
		if c.ID == "c3" {
			if c.RunID != "run-1" {
				t.Fatalf("c3: expected run_id 'run-1', got %q", c.RunID)
			}
			if c.Output != "result" {
				t.Fatalf("c3: expected output 'result', got %q", c.Output)
			}
		}
	}
}

func TestRestoreTaskBoard_Empty(t *testing.T) {
	tb := RestoreTaskBoard("s-empty", nil)
	defer tb.Close()

	if tb.ScopeID() != "s-empty" {
		t.Fatalf("expected scope s-empty, got %q", tb.ScopeID())
	}
	if len(tb.Cards()) != 0 {
		t.Fatalf("expected 0 cards, got %d", len(tb.Cards()))
	}
}

// ---------------------------------------------------------------------------
// MaxCards / CardTTL eviction
// ---------------------------------------------------------------------------

func TestBoard_WithMaxCards_EvictsTerminal(t *testing.T) {
	b := NewBoard("scope-k2-max", WithMaxCards(5))
	defer b.Close()

	for i := 0; i < 5; i++ {
		c := b.Produce("task", "p", nil)
		b.Claim(c.ID, "a")
		b.Done(c.ID, "r")
	}

	if b.Len() != 5 {
		t.Fatalf("expected 5 cards before eviction trigger, got %d", b.Len())
	}

	b.Produce("task", "p", nil)
	b.Produce("task", "p", nil)

	if b.Len() > 7 {
		t.Fatalf("expected cards to be evicted to stay near maxCards, got %d", b.Len())
	}

	pending := b.CountByStatus(CardPending, "")
	if pending < 2 {
		t.Fatalf("pending cards should not be evicted, got %d pending", pending)
	}
}

func TestBoard_WithCardTTL_EvictsExpired(t *testing.T) {
	b := NewBoard("scope-k2-ttl", WithCardTTL(50*time.Millisecond))
	defer b.Close()

	c1 := b.Produce("task", "p", nil)
	b.Claim(c1.ID, "a")
	b.Done(c1.ID, "r")

	time.Sleep(80 * time.Millisecond)

	b.Produce("task", "p", nil)

	done := b.CountByStatus(CardDone, "")
	if done != 0 {
		t.Fatalf("expected done card to be evicted after TTL, got %d done", done)
	}
	if b.CountByStatus(CardPending, "") != 1 {
		t.Fatal("pending card should survive TTL eviction")
	}
}

func TestBoard_WithMaxCards_PreservesActiveCards(t *testing.T) {
	b := NewBoard("scope-k2-active", WithMaxCards(3))
	defer b.Close()

	for i := 0; i < 5; i++ {
		b.Produce("task", "p", nil)
	}

	if b.CountByStatus(CardPending, "") != 5 {
		t.Fatalf("active (pending) cards should never be evicted, got %d", b.CountByStatus(CardPending, ""))
	}
}

// ---------------------------------------------------------------------------
// normalizePayload
// ---------------------------------------------------------------------------

func TestBoard_NormalizePayload_TypeConsistency(t *testing.T) {
	b := NewBoard("scope-k6")
	defer b.Close()

	card := b.Produce("task", "p", TaskPayload{
		TargetAgentID: "agent-1",
		Query:         "hello",
	})

	if _, ok := card.Payload.(map[string]any); !ok {
		t.Fatalf("expected Payload to be map[string]any after Produce, got %T", card.Payload)
	}

	got, _ := b.GetCardByID(card.ID)
	if _, ok := got.Payload.(map[string]any); !ok {
		t.Fatalf("expected internal Payload to be map[string]any, got %T", got.Payload)
	}

	p := PayloadMap(got.Payload)
	if p["query"] != "hello" {
		t.Fatalf("expected query='hello', got %v", p["query"])
	}
	if p["target_agent_id"] != "agent-1" {
		t.Fatalf("expected target_agent_id='agent-1', got %v", p["target_agent_id"])
	}
}

func TestBoard_NormalizePayload_DoneAlsoNormalized(t *testing.T) {
	b := NewBoard("scope-k6-done")
	defer b.Close()

	card := b.Produce("task", "p", nil)
	b.Claim(card.ID, "a")
	b.Done(card.ID, ResultPayload{Output: "result text", Error: ""})

	got, _ := b.GetCardByID(card.ID)
	if _, ok := got.Payload.(map[string]any); !ok {
		t.Fatalf("expected Done Payload to be map[string]any, got %T", got.Payload)
	}

	p := PayloadMap(got.Payload)
	if p["output"] != "result text" {
		t.Fatalf("expected output='result text', got %v", p["output"])
	}
}

func TestBoard_NormalizePayload_MapPassthrough(t *testing.T) {
	b := NewBoard("scope-k6-map")
	defer b.Close()

	original := map[string]any{"key": "val"}
	card := b.Produce("task", "p", original)

	if _, ok := card.Payload.(map[string]any); !ok {
		t.Fatalf("map[string]any should pass through normalizePayload, got %T", card.Payload)
	}
}

func TestBoard_NormalizePayload_PrimitivePassthrough(t *testing.T) {
	b := NewBoard("scope-k6-prim")
	defer b.Close()

	card := b.Produce("task", "p", "hello string")
	if s, ok := card.Payload.(string); !ok || s != "hello string" {
		t.Fatalf("string payload should pass through, got %T: %v", card.Payload, card.Payload)
	}
}

// ---------------------------------------------------------------------------
// deepCopyJSONValue
// ---------------------------------------------------------------------------

type unmarshalable struct {
	Ch chan int
}

func TestDeepCopyJSONValue_Nil(t *testing.T) {
	if deepCopyJSONValue(nil) != nil {
		t.Fatal("expected nil")
	}
}

func TestDeepCopyJSONValue_Primitive(t *testing.T) {
	if v := deepCopyJSONValue("hello"); v != "hello" {
		t.Fatalf("expected 'hello', got %v", v)
	}
	if v := deepCopyJSONValue(42); v != 42 {
		t.Fatalf("expected 42, got %v", v)
	}
	if v := deepCopyJSONValue(true); v != true {
		t.Fatalf("expected true, got %v", v)
	}
}

func TestDeepCopyJSONValue_Map(t *testing.T) {
	original := map[string]any{"a": float64(1), "b": "two"}
	cp := deepCopyJSONValue(original)
	m, ok := cp.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", cp)
	}
	if m["a"] != float64(1) || m["b"] != "two" {
		t.Fatalf("unexpected copy: %v", m)
	}

	original["a"] = float64(999)
	if m["a"] == float64(999) {
		t.Fatal("copy should be independent of original")
	}
}

func TestDeepCopyJSONValue_FailReturnsNil(t *testing.T) {
	v := deepCopyJSONValue(unmarshalable{Ch: make(chan int)})
	if v != nil {
		t.Fatalf("expected nil when marshal fails, got %v", v)
	}
}

// ---------------------------------------------------------------------------
// Index consistency across state transitions
// ---------------------------------------------------------------------------

func TestBoard_IndexConsistency_FullLifecycle(t *testing.T) {
	b := NewBoard("scope-idx-lc")
	defer b.Close()

	card := b.Produce("task", "p", map[string]any{"v": 1})

	if b.CountByStatus(CardPending, "") != 1 {
		t.Fatal("pending count should be 1 after Produce")
	}

	b.Claim(card.ID, "agent")
	if b.CountByStatus(CardPending, "") != 0 {
		t.Fatal("pending count should be 0 after Claim")
	}
	if b.CountByStatus(CardClaimed, "") != 1 {
		t.Fatal("claimed count should be 1 after Claim")
	}

	b.Done(card.ID, "result")
	if b.CountByStatus(CardClaimed, "") != 0 {
		t.Fatal("claimed count should be 0 after Done")
	}
	if b.CountByStatus(CardDone, "") != 1 {
		t.Fatal("done count should be 1 after Done")
	}

	got, ok := b.GetCardByID(card.ID)
	if !ok {
		t.Fatal("card should still be findable by ID after Done")
	}
	if got.Status != CardDone {
		t.Fatalf("expected Done status, got %s", got.Status)
	}
}

func TestBoard_IndexConsistency_FailFromPending(t *testing.T) {
	b := NewBoard("scope-idx-fp")
	defer b.Close()

	card := b.Produce("task", "p", nil)
	b.Fail(card.ID, "fail-from-pending")

	if b.CountByStatus(CardPending, "") != 0 {
		t.Fatal("pending should be 0 after Fail")
	}
	if b.CountByStatus(CardFailed, "") != 1 {
		t.Fatal("failed should be 1 after Fail")
	}
}

// ---------------------------------------------------------------------------
// Bus
// ---------------------------------------------------------------------------

func TestBoard_Bus(t *testing.T) {
	b := NewBoard("scope-bus")
	defer b.Close()

	bus := b.Bus()
	if bus == nil {
		t.Fatal("Bus() should not be nil")
	}

	ctx := context.Background()
	sub, err := bus.Subscribe(ctx, event.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ev := event.Event{
		ID:        "ev-1",
		Type:      event.EventNodeStart,
		Timestamp: time.Now(),
		Payload:   map[string]any{"node_id": "n1"},
	}
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-sub.Events():
		if got.ID != "ev-1" {
			t.Fatalf("expected event ID 'ev-1', got %q", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBoard_Bus_MultipleSubscribers(t *testing.T) {
	b := NewBoard("scope-multi")
	defer b.Close()

	ctx := context.Background()
	const n = 5
	subs := make([]event.Subscription, n)
	for i := range subs {
		s, err := b.Bus().Subscribe(ctx, event.EventFilter{})
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		subs[i] = s
	}

	ev := event.Event{ID: "ev-multi", Type: event.EventGraphStart, Timestamp: time.Now()}
	if err := b.Bus().Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for i, sub := range subs {
		select {
		case got := <-sub.Events():
			if got.ID != "ev-multi" {
				t.Fatalf("sub[%d]: expected 'ev-multi', got %q", i, got.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub[%d]: timed out", i)
		}
	}
}

func TestBoard_Close_Idempotent(t *testing.T) {
	b := NewBoard("scope-close")
	b.Close()
	b.Close()
}

// ---------------------------------------------------------------------------
// NewBoard / NewTaskBoard
// ---------------------------------------------------------------------------

func TestNewBoard(t *testing.T) {
	b := NewBoard("scope-1")
	defer b.Close()
	if b.ScopeID() != "scope-1" {
		t.Fatalf("expected scope ID 'scope-1', got %q", b.ScopeID())
	}
}

func TestNewTaskBoard_Alias(t *testing.T) {
	tb := NewTaskBoard("scope-alias")
	defer tb.Close()
	if tb.ScopeID() != "scope-alias" {
		t.Fatalf("expected scope ID 'scope-alias', got %q", tb.ScopeID())
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestBoard_ConcurrentProduceAndQuery(t *testing.T) {
	b := NewBoard("scope-cpq")
	defer b.Close()
	const n = 50

	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.Produce("task", "p", nil)
		}()
		go func() {
			defer wg.Done()
			_ = b.Query(CardFilter{Type: "task"})
		}()
	}

	wg.Wait()

	if b.Len() != n {
		t.Fatalf("expected %d cards, got %d", n, b.Len())
	}
}

func TestBoard_ConcurrentProduceClaimDone(t *testing.T) {
	b := NewBoard("scope-conc-pcd")
	defer b.Close()
	const n = 100

	var wg sync.WaitGroup
	cardIDs := make([]string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			card := b.Produce("task", fmt.Sprintf("p-%d", idx), map[string]any{"idx": idx})
			cardIDs[idx] = card.ID
		}(i)
	}
	wg.Wait()

	if b.Len() != n {
		t.Fatalf("expected %d cards, got %d", n, b.Len())
	}

	claimed := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			claimed[idx] = b.Claim(cardIDs[idx], fmt.Sprintf("c-%d", idx))
		}(i)
	}
	wg.Wait()

	for i, ok := range claimed {
		if !ok {
			t.Fatalf("claim %d failed", i)
		}
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.Done(cardIDs[idx], fmt.Sprintf("result-%d", idx))
		}(i)
	}
	wg.Wait()

	doneCards := b.Query(CardFilter{Status: CardDone})
	if len(doneCards) != n {
		t.Fatalf("expected %d done cards, got %d", n, len(doneCards))
	}
}

func TestBoard_ConcurrentWatchAndProduce(t *testing.T) {
	b := NewBoard("scope-cwp")
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const watchers = 5
	const produces = 20

	channels := make([]<-chan *Card, watchers)
	for i := 0; i < watchers; i++ {
		channels[i] = b.WatchFiltered(ctx, CardFilter{})
	}

	var wg sync.WaitGroup
	for i := 0; i < produces; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.Produce("task", "p", map[string]any{"i": idx})
		}(i)
	}
	wg.Wait()

	for wi, ch := range channels {
		count := 0
		timeout := time.After(time.Second)
	drain:
		for {
			select {
			case <-ch:
				count++
				if count == produces {
					break drain
				}
			case <-timeout:
				break drain
			}
		}
		if count != produces {
			t.Fatalf("watcher %d: expected %d events, got %d", wi, produces, count)
		}
	}
}

func TestBoard_ConcurrentBusSubscribe(t *testing.T) {
	b := NewBoard("scope-conc-bus")
	defer b.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Bus().Subscribe(ctx, event.EventFilter{})
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Subscribe failed: %v", err)
	}
}

func TestBoard_IndexConsistency_ConcurrentTransitions(t *testing.T) {
	b := NewBoard("scope-idx-conc")
	defer b.Close()
	const n = 50

	ids := make([]string, n)
	for i := 0; i < n; i++ {
		c := b.Produce("task", "p", nil)
		ids[i] = c.ID
	}

	if b.CountByStatus(CardPending, "") != n {
		t.Fatalf("expected %d pending, got %d", n, b.CountByStatus(CardPending, ""))
	}

	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(id string) {
			b.Claim(id, "a")
			b.Done(id, "r")
			done <- struct{}{}
		}(ids[i])
	}
	for i := 0; i < n; i++ {
		<-done
	}

	if b.CountByStatus(CardDone, "") != n {
		t.Fatalf("expected %d done, got %d", n, b.CountByStatus(CardDone, ""))
	}
	if b.CountByStatus(CardPending, "") != 0 {
		t.Fatalf("expected 0 pending, got %d", b.CountByStatus(CardPending, ""))
	}
	if b.CountByStatus(CardClaimed, "") != 0 {
		t.Fatalf("expected 0 claimed, got %d", b.CountByStatus(CardClaimed, ""))
	}
}
