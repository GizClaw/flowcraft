package kanban

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestBoard_New(t *testing.T) {
	t.Parallel()
	for _, ctor := range []struct {
		name string
		make func(scope string) *Board
	}{
		{"NewBoard", func(s string) *Board { return NewBoard(s) }},
		{"NewTaskBoard", func(s string) *Board { return NewTaskBoard(s) }},
	} {
		ctor := ctor
		t.Run(ctor.name, func(t *testing.T) {
			t.Parallel()
			b := ctor.make(scopeID(t))
			t.Cleanup(b.Close)
			if b.ScopeID() == "" {
				t.Fatal("ScopeID empty")
			}
		})
	}
}

func TestBoard_Close_Idempotent(t *testing.T) {
	t.Parallel()
	b := NewBoard(scopeID(t))
	b.Close()
	b.Close() // second close must not panic
}

// ---------------------------------------------------------------------------
// Deep-copy isolation: callers must not be able to mutate Board state
// through any returned Card. Covers Produce / Query / Last / RawCards /
// GetCardByID — they all use the same deepCopy path.
// ---------------------------------------------------------------------------

func TestBoard_ReturnsDeepCopies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mutate func(b *Board, original *Card)
		check  func(t *testing.T, b *Board, original *Card)
	}{
		{
			name: "Produce_then_Last",
			mutate: func(b *Board, c *Card) {
				c.Status = CardDone
				c.Meta["m"] = "mutated"
			},
			check: func(t *testing.T, b *Board, _ *Card) {
				got := b.Last(CardFilter{Type: "task"})
				if got.Status != CardPending || got.Meta["m"] != "v" {
					t.Fatalf("internal state leaked: status=%s meta=%q", got.Status, got.Meta["m"])
				}
			},
		},
		{
			name: "Query",
			mutate: func(b *Board, _ *Card) {
				for _, c := range b.Query(CardFilter{Type: "task"}) {
					c.Status = CardDone
				}
			},
			check: func(t *testing.T, b *Board, _ *Card) {
				for _, c := range b.Query(CardFilter{Type: "task"}) {
					if c.Status != CardPending {
						t.Fatalf("Query mutation leaked into board")
					}
				}
			},
		},
		{
			name: "Last",
			mutate: func(b *Board, _ *Card) {
				b.Last(CardFilter{Type: "task"}).Producer = "mutated"
			},
			check: func(t *testing.T, b *Board, _ *Card) {
				if got := b.Last(CardFilter{Type: "task"}).Producer; got == "mutated" {
					t.Fatalf("Last mutation leaked: producer=%q", got)
				}
			},
		},
		{
			name: "RawCards",
			mutate: func(b *Board, _ *Card) {
				b.RawCards()[0].Type = "mutated"
			},
			check: func(t *testing.T, b *Board, _ *Card) {
				if got := b.RawCards()[0].Type; got == "mutated" {
					t.Fatalf("RawCards mutation leaked: type=%q", got)
				}
			},
		},
		{
			name: "GetCardByID",
			mutate: func(b *Board, original *Card) {
				got, _ := b.GetCardByID(original.ID)
				got.Status = CardDone
			},
			check: func(t *testing.T, b *Board, original *Card) {
				got, _ := b.GetCardByID(original.ID)
				if got.Status != CardPending {
					t.Fatalf("GetCardByID mutation leaked: status=%s", got.Status)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoard(t)
			card := b.Produce("task", "p", map[string]any{"k": "v"}, WithMeta("m", "v"))
			tc.mutate(b, card)
			tc.check(t, b, card)
		})
	}
}

// ---------------------------------------------------------------------------
// State machine: Claim / Done / Fail transitions and validity rules.
// ---------------------------------------------------------------------------

func TestBoard_StateTransitions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		setup   func(b *Board) string
		action  func(b *Board, id string) bool
		wantOK  bool
		wantEnd CardStatus
	}{
		{"DoubleClaim_fails", func(b *Board) string {
			c := b.Produce("task", "p", nil)
			b.Claim(c.ID, "a1")
			return c.ID
		}, func(b *Board, id string) bool { return b.Claim(id, "a2") }, false, CardClaimed},

		{"DoneOnPending_fails", func(b *Board) string {
			return b.Produce("task", "p", nil).ID
		}, func(b *Board, id string) bool { return b.Done(id, "r") }, false, CardPending},

		{"DoneAfterDone_fails", func(b *Board) string {
			c := b.Produce("task", "p", nil)
			b.Claim(c.ID, "a")
			b.Done(c.ID, "r1")
			return c.ID
		}, func(b *Board, id string) bool { return b.Done(id, "r2") }, false, CardDone},

		{"FailOnPending_succeeds", func(b *Board) string {
			return b.Produce("task", "p", nil).ID
		}, func(b *Board, id string) bool { return b.Fail(id, "boom") }, true, CardFailed},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoard(t)
			id := tc.setup(b)
			if got := tc.action(b, id); got != tc.wantOK {
				t.Fatalf("action returned %v, want %v", got, tc.wantOK)
			}
			got, _ := b.GetCardByID(id)
			if got.Status != tc.wantEnd {
				t.Fatalf("end state %s, want %s", got.Status, tc.wantEnd)
			}
		})
	}
}

func TestBoard_FailFromPending_RecordsError(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	c := b.Produce("task", "p", nil)
	if !b.Fail(c.ID, "error") {
		t.Fatal("Fail on pending card should succeed")
	}
	got, _ := b.GetCardByID(c.ID)
	if got.Error != "error" {
		t.Fatalf("Error = %q, want %q", got.Error, "error")
	}
}

func TestBoard_QueryByStatus_Counts(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	c1 := b.Produce("task", "p1", "payload1")
	b.Claim(c1.ID, "a1")
	b.Done(c1.ID, "result1")

	c2 := b.Produce("task", "p2", "payload2")
	b.Claim(c2.ID, "a2")
	b.Fail(c2.ID, "oops")

	b.Produce("task", "p3", "payload3") // pending

	for status, want := range map[CardStatus]int{
		CardDone:    1,
		CardFailed:  1,
		CardPending: 1,
	} {
		if got := len(b.Query(CardFilter{Status: status})); got != want {
			t.Errorf("Query(%s)=%d, want %d", status, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// CountByStatus
// ---------------------------------------------------------------------------

func TestBoard_CountByStatus(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	c1 := b.Produce("task", "p", nil)
	c2 := b.Produce("task", "p", nil)
	b.Produce("signal", "p", nil)

	for _, c := range []struct {
		status   CardStatus
		typeName string
		want     int
		label    string
	}{
		{CardPending, "", 3, "all pending"},
		{CardPending, "task", 2, "pending tasks"},
		{CardPending, "signal", 1, "pending signals"},
	} {
		if got := b.CountByStatus(c.status, c.typeName); got != c.want {
			t.Errorf("%s: got %d, want %d", c.label, got, c.want)
		}
	}

	b.Claim(c1.ID, "a1")
	b.Done(c1.ID, "result")
	b.Fail(c2.ID, "oops")

	for _, c := range []struct {
		status   CardStatus
		typeName string
		want     int
		label    string
	}{
		{CardDone, "", 1, "after done"},
		{CardFailed, "", 1, "after fail"},
		{CardPending, "", 1, "remaining pending (signal)"},
	} {
		if got := b.CountByStatus(c.status, c.typeName); got != c.want {
			t.Errorf("%s: got %d, want %d", c.label, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// GetCardByID
// ---------------------------------------------------------------------------

func TestBoard_GetCardByID_NotFound(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	if _, ok := b.GetCardByID("nonexistent"); ok {
		t.Fatal("expected ok=false")
	}
}

func TestBoard_GetCardByID_AfterRemap(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	c := b.Produce("task", "p", nil)
	const newID = "remapped-id"
	b.RemapCardID(c.ID, newID)

	if _, ok := b.GetCardByID(c.ID); ok {
		t.Fatal("old ID should not be findable after remap")
	}
	got, ok := b.GetCardByID(newID)
	if !ok {
		t.Fatal("new ID should be findable after remap")
	}
	if got.ID != newID {
		t.Fatalf("ID = %q, want %q", got.ID, newID)
	}
}

// ---------------------------------------------------------------------------
// Watch / WatchFiltered
// ---------------------------------------------------------------------------

func TestBoard_Watch_FullLifecycle(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := b.WatchFiltered(ctx, CardFilter{Type: "task"})

	c := b.Produce("task", "orch", map[string]any{"n": 1})
	if got := waitCard(t, ch, "produce"); got.ID != c.ID || got.Status != CardPending {
		t.Fatalf("produce: ID=%q status=%s", got.ID, got.Status)
	}

	b.Claim(c.ID, "agent-1")
	if got := waitCard(t, ch, "claim"); got.Status != CardClaimed || got.Consumer != "agent-1" {
		t.Fatalf("claim: status=%s consumer=%q", got.Status, got.Consumer)
	}

	b.Done(c.ID, "result-data")
	if got := waitCard(t, ch, "done"); got.Status != CardDone {
		t.Fatalf("done: status=%s", got.Status)
	}

	c2 := b.Produce("task", "orch", map[string]any{"n": 2})
	waitCard(t, ch, "produce c2")
	b.Claim(c2.ID, "agent-2")
	waitCard(t, ch, "claim c2")
	b.Fail(c2.ID, "agent error")
	if got := waitCard(t, ch, "fail"); got.Status != CardFailed || got.Error != "agent error" {
		t.Fatalf("fail: status=%s err=%q", got.Status, got.Error)
	}
}

func TestBoard_Watch_DeliversSnapshotsNotLivePointers(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := b.WatchFiltered(ctx, CardFilter{})

	c := b.Produce("task", "p", map[string]any{"v": 1}, WithMeta("key", "val"))
	got := waitCard(t, ch, "produce")
	b.Claim(c.ID, "agent")

	if got.Status != CardPending {
		t.Fatal("snapshot must remain Pending after the source card is Claimed")
	}
	if got.Meta["key"] != "val" {
		t.Fatal("Meta must be deep-copied into snapshot")
	}
}

func TestBoard_Watch_FilteringByType(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	taskCh := b.WatchFiltered(ctx, CardFilter{Type: "task"})
	resultCh := b.WatchFiltered(ctx, CardFilter{Type: "result"})
	allCh := b.WatchFiltered(ctx, CardFilter{})

	b.Produce("task", "p", nil)
	b.Produce("result", "p", nil)
	b.Produce("signal", "p", nil)

	if got := waitCard(t, taskCh, "task"); got.Type != "task" {
		t.Fatalf("taskCh got %s", got.Type)
	}
	if got := waitCard(t, resultCh, "result"); got.Type != "result" {
		t.Fatalf("resultCh got %s", got.Type)
	}

	count := 0
	timeout := time.After(time.Second)
drain:
	for count < 3 {
		select {
		case <-allCh:
			count++
		case <-timeout:
			break drain
		}
	}
	if count != 3 {
		t.Fatalf("allCh got %d, want 3", count)
	}
}

func TestBoard_Watch_CleanupOnContextCancel(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.WatchFiltered(ctx, CardFilter{})
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel close after context cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after cancel")
	}

	// Producing after the watcher is gone must not panic.
	b.Produce("task", "p", nil)
}

func TestBoard_Watch_ClosesOnBoardClose(t *testing.T) {
	t.Parallel()
	b := NewBoard(scopeID(t))
	ch := b.Watch(context.Background())
	b.Close()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel close after Board.Close")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after Board.Close")
	}
}

// Bug 1 (P0): unbounded watcher queue must deliver every event regardless of
// consumer pace.
func TestBoard_Watch_SlowConsumer_NoEventLoss(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := b.WatchFiltered(ctx, CardFilter{})

	const total = watchBufSize * 4 // intentionally larger than the legacy bound
	for i := 0; i < total; i++ {
		b.Produce("task", "p", nil)
	}

	drained := 0
	timeout := time.After(2 * time.Second)
drain:
	for drained < total {
		select {
		case <-ch:
			drained++
		case <-timeout:
			break drain
		}
	}
	if drained != total {
		t.Fatalf("got %d/%d events; queue dropped events", drained, total)
	}
	if got := b.WatcherDropped(); got != 0 {
		t.Fatalf("WatcherDropped should be 0 for a live watcher, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Cards / Timeline / Topology view methods.
//
// These views must filter internal-only card types ("result", "cron_rule")
// out of the public API. Bug 2 (issue #28) was a regression here.
// ---------------------------------------------------------------------------

func TestBoard_Cards_FiltersInternalTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		seed   func(b *Board)
		hidden string
	}{
		{
			name: "result cards hidden",
			seed: func(b *Board) {
				b.Produce("task", "agent-main", TaskPayload{Query: "q1", TargetAgentID: "t1"})
				b.Produce("result", "agent-1", ResultPayload{Output: "r1"},
					WithConsumer("agent-main"), WithMeta("task_card_id", "c1"))
				b.Produce("task", "agent-main", TaskPayload{Query: "q2", TargetAgentID: "t2"})
			},
			hidden: "result",
		},
		{
			name: "cron_rule cards hidden",
			seed: func(b *Board) {
				b.Produce("task", "agent-main", TaskPayload{Query: "real task", TargetAgentID: "t1"})
				b.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
					AgentID: "agent-main", ScheduleID: "sched-1",
					Cron: "0 9 * * *", Query: "daily",
				}, WithMeta("schedule_id", "sched-1"))
			},
			hidden: cardTypeCronRule,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoard(t)
			tc.seed(b)
			for _, c := range b.Cards() {
				if c.Type == tc.hidden {
					t.Fatalf("%s should be filtered out of Cards()", tc.hidden)
				}
			}
		})
	}
}

func TestBoard_Cards_ExtractsPayloadFields(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	c := b.Produce("task", "orch", TaskPayload{Query: "the query", TargetAgentID: "the_tpl"})
	b.Claim(c.ID, "agent-1")
	b.Done(c.ID, map[string]any{
		"query":           "the query",
		"target_agent_id": "the_tpl",
		"output":          "the output",
		"run_id":          "run-abc",
	})

	cards := b.Cards()
	if len(cards) != 1 {
		t.Fatalf("Cards()=%d, want 1", len(cards))
	}
	got := cards[0]
	if got.Query != "the query" || got.TargetAgentID != "the_tpl" ||
		got.Output != "the output" || got.RunID != "run-abc" {
		t.Fatalf("unexpected extracted fields: %+v", got)
	}
}

func TestBoard_Timeline_FiltersInternalTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		seed func(b *Board)
	}{
		{
			name: "result hidden",
			seed: func(b *Board) {
				b.Produce("task", "orch", TaskPayload{Query: "q1"})
				b.Produce("result", "agent", ResultPayload{Output: "r1"})
			},
		},
		{
			name: "cron_rule hidden",
			seed: func(b *Board) {
				b.Produce("task", "orch", TaskPayload{Query: "q1"})
				b.Produce(cardTypeCronRule, "scheduler", cronRulePayload{
					AgentID: "orch", ScheduleID: "sched-1",
					Cron: "0 9 * * *", Query: "daily",
				}, WithMeta("schedule_id", "sched-1"))
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoard(t)
			tc.seed(b)
			tl := b.Timeline()
			if len(tl) != 1 {
				t.Fatalf("Timeline()=%d, want 1", len(tl))
			}
			if tl[0].Type == "result" || tl[0].Type == cardTypeCronRule {
				t.Fatalf("Timeline leaked %s", tl[0].Type)
			}
		})
	}
}

func TestBoard_Topology(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	b.Produce("task", "agent-main", TaskPayload{Query: "q1"}, WithConsumer("agent-1"))
	b.Produce("task", "agent-main", TaskPayload{Query: "q2"}, WithConsumer("agent-2"))

	topo := b.Topology()
	if len(topo.Nodes) != 3 {
		t.Fatalf("Nodes=%d, want 3 (producer + 2 consumers)", len(topo.Nodes))
	}
	if len(topo.Edges) != 2 {
		t.Fatalf("Edges=%d, want 2", len(topo.Edges))
	}
}

// ---------------------------------------------------------------------------
// Payload extraction helpers (extractPayloadFields / extractRunID).
// ---------------------------------------------------------------------------

func TestExtractPayloadFieldsPublic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		in              any
		query, tid, out string
	}{
		{"struct", TaskPayload{Query: "q", TargetAgentID: "t"}, "q", "t", ""},
		{"map", map[string]any{"query": "q2", "output": "o2"}, "q2", "", "o2"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			q, tid, o := ExtractPayloadFieldsPublic(c.in)
			if q != c.query || tid != c.tid || o != c.out {
				t.Fatalf("got q=%q tid=%q o=%q; want q=%q tid=%q o=%q",
					q, tid, o, c.query, c.tid, c.out)
			}
		})
	}
}

func TestExtractRunID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"map_with_run_id", map[string]any{"run_id": "r1"}, "r1"},
		{"map_without_run_id", map[string]any{"other": "val"}, ""},
		{"struct", TaskPayload{Query: "q"}, ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := extractRunID(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MaxCards / CardTTL eviction
// ---------------------------------------------------------------------------

func TestBoard_WithMaxCards_EvictsTerminal(t *testing.T) {
	t.Parallel()
	b := NewBoard(scopeID(t), WithMaxCards(5))
	t.Cleanup(b.Close)

	for i := 0; i < 5; i++ {
		c := b.Produce("task", "p", nil)
		b.Claim(c.ID, "a")
		b.Done(c.ID, "r")
	}
	if b.Len() != 5 {
		t.Fatalf("before extra produces: Len()=%d, want 5", b.Len())
	}

	b.Produce("task", "p", nil)
	b.Produce("task", "p", nil)

	if b.Len() > 7 {
		t.Fatalf("eviction did not bound size: Len()=%d", b.Len())
	}
	if pending := b.CountByStatus(CardPending, ""); pending < 2 {
		t.Fatalf("pending cards must not be evicted, got %d pending", pending)
	}
}

func TestBoard_WithCardTTL_EvictsExpired(t *testing.T) {
	t.Parallel()
	b := NewBoard(scopeID(t), WithCardTTL(50*time.Millisecond))
	t.Cleanup(b.Close)

	c := b.Produce("task", "p", nil)
	b.Claim(c.ID, "a")
	b.Done(c.ID, "r")

	time.Sleep(80 * time.Millisecond)
	b.Produce("task", "p", nil) // triggers eviction sweep

	if got := b.CountByStatus(CardDone, ""); got != 0 {
		t.Fatalf("done card not evicted after TTL: %d remain", got)
	}
	if got := b.CountByStatus(CardPending, ""); got != 1 {
		t.Fatalf("pending must survive TTL eviction, got %d", got)
	}
}

func TestBoard_WithMaxCards_PreservesActiveCards(t *testing.T) {
	t.Parallel()
	b := NewBoard(scopeID(t), WithMaxCards(3))
	t.Cleanup(b.Close)

	for i := 0; i < 5; i++ {
		b.Produce("task", "p", nil)
	}
	if got := b.CountByStatus(CardPending, ""); got != 5 {
		t.Fatalf("active (pending) cards must never be evicted, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// normalizePayload — Produce/Done normalize to map[string]any (or pass
// through primitives) so downstream callers see one consistent shape.
// ---------------------------------------------------------------------------

func TestBoard_NormalizePayload(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload any
		check   func(t *testing.T, got *Card)
	}{
		{
			name:    "struct -> map",
			payload: TaskPayload{TargetAgentID: "agent-1", Query: "hello"},
			check: func(t *testing.T, c *Card) {
				m, ok := c.Payload.(map[string]any)
				if !ok {
					t.Fatalf("Payload type = %T, want map[string]any", c.Payload)
				}
				if m["query"] != "hello" || m["target_agent_id"] != "agent-1" {
					t.Fatalf("missing fields after normalize: %v", m)
				}
			},
		},
		{
			name:    "map passthrough",
			payload: map[string]any{"key": "val"},
			check: func(t *testing.T, c *Card) {
				if _, ok := c.Payload.(map[string]any); !ok {
					t.Fatalf("map should pass through, got %T", c.Payload)
				}
			},
		},
		{
			name:    "primitive passthrough",
			payload: "hello string",
			check: func(t *testing.T, c *Card) {
				s, ok := c.Payload.(string)
				if !ok || s != "hello string" {
					t.Fatalf("string should pass through, got %T: %v", c.Payload, c.Payload)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBoard(t)
			tc.check(t, b.Produce("task", "p", tc.payload))
		})
	}
}

func TestBoard_NormalizePayload_DoneAlsoNormalized(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	c := b.Produce("task", "p", nil)
	b.Claim(c.ID, "a")
	b.Done(c.ID, ResultPayload{Output: "result text"})

	got, _ := b.GetCardByID(c.ID)
	m, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Done payload should normalize to map, got %T", got.Payload)
	}
	if m["output"] != "result text" {
		t.Fatalf("output = %v, want 'result text'", m["output"])
	}
}

// ---------------------------------------------------------------------------
// deepCopyJSONValue
// ---------------------------------------------------------------------------

type unmarshalable struct {
	Ch chan int
}

func TestDeepCopyJSONValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, nil},
		{"string", "hello", "hello"},
		{"int", 42, 42},
		{"bool", true, true},
		{"unmarshalable", unmarshalable{Ch: make(chan int)}, nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := deepCopyJSONValue(c.in); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDeepCopyJSONValue_MapIsIndependent(t *testing.T) {
	t.Parallel()
	original := map[string]any{"a": float64(1), "b": "two"}
	cp, ok := deepCopyJSONValue(original).(map[string]any)
	if !ok {
		t.Fatalf("copy is not a map")
	}
	if cp["a"] != float64(1) || cp["b"] != "two" {
		t.Fatalf("copy missing fields: %v", cp)
	}
	original["a"] = float64(999)
	if cp["a"] == float64(999) {
		t.Fatal("mutation to original leaked into copy")
	}
}

// ---------------------------------------------------------------------------
// Index consistency: cardIndex / statusIndex stay in sync with state.
// (Concurrent variant lives in board_concurrency_test.go.)
// ---------------------------------------------------------------------------

func TestBoard_IndexConsistency_FullLifecycle(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	c := b.Produce("task", "p", map[string]any{"v": 1})
	if got := b.CountByStatus(CardPending, ""); got != 1 {
		t.Fatalf("pending after Produce = %d, want 1", got)
	}

	b.Claim(c.ID, "agent")
	if b.CountByStatus(CardPending, "") != 0 || b.CountByStatus(CardClaimed, "") != 1 {
		t.Fatal("pending/claimed indexes inconsistent after Claim")
	}

	b.Done(c.ID, "result")
	if b.CountByStatus(CardClaimed, "") != 0 || b.CountByStatus(CardDone, "") != 1 {
		t.Fatal("claimed/done indexes inconsistent after Done")
	}

	got, ok := b.GetCardByID(c.ID)
	if !ok || got.Status != CardDone {
		t.Fatalf("post-Done lookup mismatch: ok=%v status=%s", ok, got.Status)
	}
}

func TestBoard_IndexConsistency_FailFromPending(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	c := b.Produce("task", "p", nil)
	b.Fail(c.ID, "fail-from-pending")

	if b.CountByStatus(CardPending, "") != 0 {
		t.Fatal("pending must drop to 0 after Fail")
	}
	if b.CountByStatus(CardFailed, "") != 1 {
		t.Fatal("failed must rise to 1 after Fail")
	}
}

// ---------------------------------------------------------------------------
// Bus — generic publish/subscribe wiring.
// (Kanban-specific event payloads are tested in events_test.go.)
// ---------------------------------------------------------------------------

func TestBoard_Bus_PublishesToSubscriber(t *testing.T) {
	t.Parallel()
	b := newBoard(t)
	if b.Bus() == nil {
		t.Fatal("Bus() should not be nil")
	}

	ctx := context.Background()
	sub, err := b.Bus().Subscribe(ctx, event.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	ev := event.Event{
		ID:        "ev-1",
		Type:      event.EventNodeStart,
		Timestamp: time.Now(),
		Payload:   map[string]any{"node_id": "n1"},
	}
	if err := b.Bus().Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-sub.Events():
		if got.ID != "ev-1" {
			t.Fatalf("got ID=%q, want 'ev-1'", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBoard_Bus_FanOutsToMultipleSubscribers(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	ctx := context.Background()
	const n = 5
	subs := make([]event.LegacySubscription, n)
	for i := range subs {
		s, err := b.Bus().Subscribe(ctx, event.EventFilter{})
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		subs[i] = s
		t.Cleanup(func() { _ = s.Close() })
	}

	ev := event.Event{ID: "ev-multi", Type: event.EventGraphStart, Timestamp: time.Now()}
	if err := b.Bus().Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for i, sub := range subs {
		select {
		case got := <-sub.Events():
			if got.ID != "ev-multi" {
				t.Errorf("sub[%d]: got ID=%q, want 'ev-multi'", i, got.ID)
			}
		case <-time.After(time.Second):
			t.Errorf("sub[%d]: timed out", i)
		}
	}
}
