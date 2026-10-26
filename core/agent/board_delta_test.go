package agent_test

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestBoardDeltaReplayMatchesSnapshot is the property the whole incremental
// checkpoint design rests on: replaying the deltas a board reported between
// marks onto a base snapshot must reproduce the board's own state, for any
// sequence of writes.
func TestBoardDeltaReplayMatchesSnapshot(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	board := agent.NewBoard()
	base := board.Snapshot()
	mark := board.Mark()
	replayed := agent.RestoreBoard(base)

	for step := range 400 {
		applyRandomBoardOp(t, rng, board)
		if step%7 != 0 {
			continue
		}
		board.Since(mark).ApplyTo(replayed)
		mark = board.Mark()
	}
	// The writes after the last sampled boundary still have to persist.
	board.Since(mark).ApplyTo(replayed)

	if got, want := replayed.Snapshot(), board.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed state diverged\n got: %#v\nwant: %#v", got, want)
	}
}

// TestBoardDeltaDurableReplayMatchesSnapshot replays the deltas through their
// JSON form — the shape a checkpoint store persists — and compares against the
// board's own state put through the same encoder. Numbers become float64 on
// the way, exactly as they already do for a full checkpoint today.
func TestBoardDeltaDurableReplayMatchesSnapshot(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	board := agent.NewBoard()
	base := board.Snapshot()
	mark := board.Mark()
	replayed := agent.RestoreBoard(base)

	for step := range 300 {
		applyRandomBoardOp(t, rng, board)
		if step%5 != 0 {
			continue
		}
		raw, err := json.Marshal(board.Since(mark))
		if err != nil {
			t.Fatalf("step %d: marshal delta: %v", step, err)
		}
		var decoded agent.BoardDelta
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("step %d: unmarshal delta: %v", step, err)
		}
		decoded.ApplyTo(replayed)
		mark = board.Mark()
	}
	raw, err := json.Marshal(board.Since(mark))
	if err != nil {
		t.Fatalf("final delta: %v", err)
	}
	var tail agent.BoardDelta
	if err := json.Unmarshal(raw, &tail); err != nil {
		t.Fatalf("final delta: %v", err)
	}
	tail.ApplyTo(replayed)

	want := roundTripSnapshot(t, board.Snapshot())
	if got := replayed.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("durable replay diverged\n got: %#v\nwant: %#v", got, want)
	}
}

func roundTripSnapshot(t *testing.T, snap *agent.BoardSnapshot) *agent.BoardSnapshot {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var out agent.BoardSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	return &out
}

func applyRandomBoardOp(t *testing.T, rng *rand.Rand, board *agent.Board) {
	t.Helper()
	key := []string{"a", "b", "c"}[rng.Intn(3)]
	// Two channels, so a channel created after the mark (base 0) and the
	// per-channel loop are both exercised.
	channel := []string{agent.MainChannel, "tools"}[rng.Intn(2)]
	switch rng.Intn(7) {
	case 0:
		board.SetVar(key, rng.Intn(100))
	case 1:
		board.SetVar(key, map[string]any{"n": rng.Intn(10), "tags": []any{"x", "y"}})
	case 2:
		board.DeleteVar(key)
	case 3:
		board.AppendChannelMessage(channel,
			message.NewTextMessage(message.RoleUser, []string{"hi", "there"}[rng.Intn(2)]))
	case 4:
		board.PopChannelMessage(channel)
	case 5:
		// A wholesale set may keep the length identical, which length
		// comparison alone cannot see.
		board.SetChannel(channel, []message.Message{
			message.NewTextMessage(message.RoleAssistant, "replaced"),
		})
	case 6:
		if err := board.AppendSliceVar(key, rng.Intn(10)); err == nil {
			board.UpdateSliceVarItem(key, func(any) bool { return true },
				func(any) any { return "updated" })
		}
	}
}

// TestBoardSinceReportsWholesaleChannelSet proves a same-length replacement is
// reported: a channel's length is not enough to describe its identity.
func TestBoardSinceReportsWholesaleChannelSet(t *testing.T) {
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "one"))
	mark := board.Mark()

	board.SetChannel(agent.MainChannel, []message.Message{
		message.NewTextMessage(message.RoleAssistant, "different"),
	})

	delta := board.Since(mark)
	if got := delta.Replaced()[agent.MainChannel]; len(got) != 1 ||
		got[0].Content.Text() != "different" {
		t.Fatalf("replaced = %#v, want the wholesale set", got)
	}
	if delta.Empty() {
		t.Fatal("delta is empty after a wholesale channel set")
	}
}

// TestBoardSinceAfterRestoreIsConservative pins the stale-mark rule: a restore
// rewrites the whole state, so a mark taken before it must yield the state
// again rather than an empty delta that would silently drop it.
func TestBoardSinceAfterRestoreIsConservative(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("kept", "value")
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "one"))
	mark := board.Mark()

	snapshot := board.Snapshot()
	board.RestoreFrom(snapshot)

	delta := board.Since(mark)
	if delta.Empty() {
		t.Fatal("delta after a restore is empty; a stale mark would drop state")
	}
	if got, ok := delta.Vars()["kept"]; !ok || got != "value" {
		t.Fatalf("delta vars = %#v, want the restored key", delta.Vars())
	}

	// A mark taken after the restore is clean, which is what parallel branch
	// seeding relies on.
	fresh := board.Mark()
	board.SetVar("new", 1)
	after := board.Since(fresh)
	if _, ok := after.Vars()["kept"]; ok {
		t.Fatalf("inherited key reported after a post-restore mark: %#v", after.Vars())
	}
	if got := after.Vars()["new"]; got != 1 {
		t.Fatalf("new key = %v, want 1", got)
	}
}

func TestBoardDeltaJSONRoundTrip(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("gone", "x")
	board.SetVar("kept", []any{"a", "b"})
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "one"))
	mark := board.Mark()

	board.DeleteVar("gone")
	board.SetVar("kept", []any{"a", "b", "c"})
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "two"))

	raw, err := json.Marshal(board.Since(mark))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded agent.BoardDelta
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := decoded.Removed(); len(got) != 1 || got[0] != "gone" {
		t.Fatalf("removed = %v, want [gone]", got)
	}
	if got := decoded.Vars()["kept"]; !reflect.DeepEqual(got, []any{"a", "b", "c"}) {
		t.Fatalf("kept = %#v", got)
	}
	if got := decoded.Appends()[agent.MainChannel]; len(got) != 1 ||
		got[0].Content.Text() != "two" {
		t.Fatalf("appends = %#v", got)
	}
}

// TestBoardDeltaApplyToOwnsItsValues proves a delta applied to a board copies:
// mutating the delta's source afterwards must not reach the receiving board.
func TestBoardDeltaApplyToOwnsItsValues(t *testing.T) {
	source := agent.NewBoard()
	mark := source.Mark()
	value := map[string]any{"n": 1}
	source.SetVar("v", value)
	source.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "one"))

	target := agent.NewBoard()
	source.Since(mark).ApplyTo(target)

	value["n"] = 2
	if got, _ := target.Var("v"); !reflect.DeepEqual(got, map[string]any{"n": 1}) {
		t.Fatalf("target var = %#v, want an independent copy", got)
	}
}

func TestAuditBoardValuesCopiesOnRead(t *testing.T) {
	agent.AuditBoardValues(true)
	t.Cleanup(func() { agent.AuditBoardValues(false) })

	board := agent.NewBoard()
	stored := map[string]any{"n": 1}
	board.SetVar("v", stored)

	read, _ := board.Var("v")
	read.(map[string]any)["n"] = 99
	if got, _ := board.Var("v"); !reflect.DeepEqual(got, map[string]any{"n": 1}) {
		t.Fatalf("board var = %#v, want the audited copy to be detached", got)
	}
	vars := board.Vars()
	vars["v"].(map[string]any)["n"] = 99
	if got, _ := board.Var("v"); !reflect.DeepEqual(got, map[string]any{"n": 1}) {
		t.Fatalf("board var = %#v after Vars mutation", got)
	}
}

// TestBoardSinceReportsVarsAndChannelsDroppedByRestore pins the other half of
// the stale-mark rule: a restore can *remove* state, and a delta taken before it
// must say so. Marking only what came back would leave the replay holding keys
// and channels the board no longer has.
func TestBoardSinceReportsVarsAndChannelsDroppedByRestore(t *testing.T) {
	board := agent.NewBoard()
	board.SetVar("keep", 1)
	board.SetVar("drop", 2)
	board.AppendChannelMessage("tools", message.NewTextMessage(message.RoleTool, "t1"))
	board.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "hi"))
	before := board.Snapshot()
	mark := board.Mark()

	// The restore carries neither "drop" nor the "tools" channel.
	board.RestoreFrom(&agent.BoardSnapshot{
		Vars:     map[string]any{"keep": 1},
		Channels: map[string][]message.Message{agent.MainChannel: {message.NewTextMessage(message.RoleUser, "hi")}},
	})

	delta := board.Since(mark)
	if got := delta.Removed(); len(got) != 1 || got[0] != "drop" {
		t.Fatalf("removed = %v, want [drop]: a restore must not shrink the delta", got)
	}
	if msgs, ok := delta.Replaced()["tools"]; !ok || msgs != nil {
		t.Fatalf("replaced[tools] = %v (present=%v), want an explicit removal", msgs, ok)
	}

	replayed := agent.RestoreBoard(before)
	delta.ApplyTo(replayed)
	if got, want := replayed.Snapshot(), board.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("replay diverged from the restored board\n got: %#v\nwant: %#v", got, want)
	}
	if _, ok := replayed.Var("drop"); ok {
		t.Fatal("replay kept a var the restore removed")
	}
	if got := len(replayed.Channel("tools")); got != 0 {
		t.Fatalf("replay kept %d messages on a channel the restore removed", got)
	}
}

// TestSnapshotDelta covers the shapes a session turn produces: a changed var, a
// removed var, a channel that grew, one that was rewritten, and one that
// disappeared.
func TestSnapshotDelta(t *testing.T) {
	one := message.NewTextMessage(message.RoleUser, "one")
	two := message.NewTextMessage(message.RoleAssistant, "two")
	other := message.NewTextMessage(message.RoleTool, "other")

	base := &agent.BoardSnapshot{
		Vars: map[string]any{"kept": "same", "gone": 1, "changed": "a"},
		Channels: map[string][]message.Message{
			agent.MainChannel: {one},
			"tools":           {one, two},
			"dropped":         {one},
		},
	}
	next := &agent.BoardSnapshot{
		Vars: map[string]any{"kept": "same", "changed": "b"},
		Channels: map[string][]message.Message{
			agent.MainChannel: {one, two},
			"tools":           {other},
		},
	}

	delta := agent.SnapshotDelta(base, next)
	if got := delta.Vars(); len(got) != 1 || got["changed"] != "b" {
		t.Fatalf("vars = %#v, want only the changed key", got)
	}
	if got := delta.Removed(); len(got) != 1 || got[0] != "gone" {
		t.Fatalf("removed = %v, want [gone]", got)
	}
	if got := delta.Appends()[agent.MainChannel]; len(got) != 1 || got[0].Content.Text() != "two" {
		t.Fatalf("main appends = %#v, want only the new message", got)
	}
	if got, ok := delta.Replaced()["tools"]; !ok || len(got) != 1 || got[0].Content.Text() != "other" {
		t.Fatalf("tools replacement = %#v, want the rewritten channel", got)
	}
	if got, ok := delta.Replaced()["dropped"]; !ok || got != nil {
		t.Fatalf("dropped channel = %#v (present=%v), want an explicit removal", got, ok)
	}

	replayed := base
	delta.ApplyToSnapshot(replayed)
	if !reflect.DeepEqual(replayed, next) {
		t.Fatalf("replay = %#v, want %#v", replayed, next)
	}
}
