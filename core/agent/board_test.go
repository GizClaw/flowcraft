package agent_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// imageURLPart builds an ImagePart from a URL for tests that exercise
// multi-part messages.
func imageURLPart(t *testing.T, rawURL string) message.ImagePart {
	t.Helper()
	source, err := media.NewImageURL(rawURL, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL(%q): %v", rawURL, err)
	}
	return message.ImagePart{Source: source}
}

func TestBoard_NewBoardHasEmptyMainChannel(t *testing.T) {
	b := agent.NewBoard()
	if got := b.Channel(agent.MainChannel); got != nil {
		t.Errorf("MainChannel = %+v, want nil for empty channel", got)
	}
}

func TestBoard_AppendChannelMessage(t *testing.T) {
	b := agent.NewBoard()
	m := message.NewTextMessage(message.RoleUser, "hello")
	b.AppendChannelMessage(agent.MainChannel, m)

	got := b.Channel(agent.MainChannel)
	if len(got) != 1 || got[0].Content.Text() != "hello" {
		t.Errorf("Channel = %+v, want [hello]", got)
	}
}

func TestBoard_PopChannelMessage(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "first"))
	b.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleUser, "second"))

	popped, ok := b.PopChannelMessage(agent.MainChannel)
	if !ok || popped.Content.Text() != "second" {
		t.Fatalf("Pop = (%q, %v), want second/true", popped.Content.Text(), ok)
	}
	got := b.Channel(agent.MainChannel)
	if len(got) != 1 || got[0].Content.Text() != "first" {
		t.Fatalf("Channel = %+v, want [first]", got)
	}
	if _, ok := b.PopChannelMessage("missing"); ok {
		t.Fatalf("Pop on missing channel must report false")
	}
}

func TestBoard_SetChannel_DefensiveCopy(t *testing.T) {
	b := agent.NewBoard()
	in := []message.Message{
		message.NewTextMessage(message.RoleUser, "a"),
		message.NewTextMessage(message.RoleUser, "b"),
	}
	b.SetChannel("alt", in)

	// Mutate caller-owned slice; board copy must be unaffected.
	in[0] = message.NewTextMessage(message.RoleUser, "MUTATED")

	got := b.Channel("alt")
	if got[0].Content.Text() != "a" {
		t.Errorf("SetChannel did not defensively copy; got %q after caller mutation", got[0].Content.Text())
	}
}

func TestBoard_SetChannel_DeepCopiesMessageParts(t *testing.T) {
	b := agent.NewBoard()
	in := []message.Message{
		{
			Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "a"},
				imageURLPart(t, "https://img.example.com/a.png"),
			}},
		},
	}
	b.SetChannel("alt", in)

	// Parts are interface values: caller-side mutation means swapping
	// the slice element, which a defensive copy must not observe.
	in[0].Content.Parts[0] = message.TextPart{Text: "MUTATED"}
	in[0].Content.Parts[1] = imageURLPart(t, "https://img.example.com/MUTATED.png")

	got := b.Channel("alt")
	if text := got[0].Content.Parts[0].(message.TextPart).Text; text != "a" {
		t.Errorf("SetChannel leaked caller part mutation: %q", text)
	}
	if url := got[0].Content.Parts[1].(message.ImagePart).Source.URL(); url != "https://img.example.com/a.png" {
		t.Errorf("SetChannel leaked caller media mutation: %q", url)
	}
}

func TestBoard_ChannelDefensiveCopyOnRead(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage("alt", message.NewTextMessage(message.RoleUser, "x"))

	got := b.Channel("alt")
	got[0] = message.NewTextMessage(message.RoleUser, "MUTATED")

	again := b.Channel("alt")
	if again[0].Content.Text() != "x" {
		t.Errorf("Channel returned a non-defensive view; mutation leaked: %q", again[0].Content.Text())
	}
}

func TestBoard_ChannelDeepCopiesMessagePartsOnRead(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage("alt", message.Message{
		Role: message.RoleUser,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "x"},
			imageURLPart(t, "https://img.example.com/x.png"),
		}},
	})

	got := b.Channel("alt")
	got[0].Content.Parts[0] = message.TextPart{Text: "MUTATED"}
	got[0].Content.Parts[1] = imageURLPart(t, "https://img.example.com/MUTATED.png")

	again := b.Channel("alt")
	if text := again[0].Content.Parts[0].(message.TextPart).Text; text != "x" {
		t.Errorf("Channel leaked part mutation: %q", text)
	}
	if url := again[0].Content.Parts[1].(message.ImagePart).Source.URL(); url != "https://img.example.com/x.png" {
		t.Errorf("Channel leaked media mutation: %q", url)
	}
}

func TestBoard_VarsRoundTrip(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("k", "v")
	b.SetVar("n", 42)

	if got, _ := b.GetVar("k"); got != "v" {
		t.Errorf("k = %v", got)
	}
	if got := b.GetVarString("k"); got != "v" {
		t.Errorf("GetVarString = %q", got)
	}
	if got := b.GetVarString("n"); got != "" {
		t.Errorf("GetVarString on non-string should be empty; got %q", got)
	}
	if got := b.GetVarString("missing"); got != "" {
		t.Errorf("GetVarString on missing key should be empty; got %q", got)
	}

	if v, ok := agent.GetTyped[int](b, "n"); !ok || v != 42 {
		t.Errorf("GetTyped[int] = (%v, %v)", v, ok)
	}
	if _, ok := agent.GetTyped[bool](b, "n"); ok {
		t.Error("GetTyped should fail when types disagree")
	}
}

func TestBoard_VarsReturnsSnapshot(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("k", "v")

	got := b.Vars()
	got["k"] = "MUTATED"

	if v, _ := b.GetVar("k"); v != "v" {
		t.Errorf("Board mutated through Vars copy: got %v", v)
	}
}

func TestBoard_AppendSliceVar_NewKey(t *testing.T) {
	b := agent.NewBoard()
	if err := b.AppendSliceVar("xs", "first"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := b.AppendSliceVar("xs", "second"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v, _ := b.GetVar("xs")
	xs, ok := v.([]any)
	if !ok || len(xs) != 2 {
		t.Fatalf("xs = %+v, want []any{first, second}", v)
	}
	if xs[0] != "first" || xs[1] != "second" {
		t.Errorf("AppendSliceVar order wrong: %+v", xs)
	}
}

func TestBoard_AppendSliceVar_ConflictReturnsError(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("not_slice", "string")

	err := b.AppendSliceVar("not_slice", "x")
	if err == nil {
		t.Error("AppendSliceVar onto non-[]any value should return error")
	}
}

func TestBoard_UpdateSliceVarItem(t *testing.T) {
	b := agent.NewBoard()
	_ = b.AppendSliceVar("xs", 1)
	_ = b.AppendSliceVar("xs", 2)
	_ = b.AppendSliceVar("xs", 3)

	b.UpdateSliceVarItem("xs",
		func(v any) bool { return v == 2 },
		func(any) any { return 99 },
	)

	v, _ := b.GetVar("xs")
	xs := v.([]any)
	if xs[0] != 1 || xs[1] != 99 || xs[2] != 3 {
		t.Errorf("UpdateSliceVarItem: %+v", xs)
	}
}

func TestBoard_UpdateSliceVarItem_MissingKeyIsNoOp(t *testing.T) {
	b := agent.NewBoard()
	b.UpdateSliceVarItem("missing",
		func(any) bool { return true },
		func(any) any { return "x" },
	)
	if v, ok := b.GetVar("missing"); ok {
		t.Errorf("UpdateSliceVarItem on missing key should not create the key; got %v", v)
	}
}

func TestBoard_ChannelsCopy_DeepCopiesPerChannel(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage("a", message.NewTextMessage(message.RoleUser, "alpha"))
	b.AppendChannelMessage("b", message.NewTextMessage(message.RoleUser, "beta"))

	cp := b.ChannelsCopy()
	cp["a"][0] = message.NewTextMessage(message.RoleUser, "MUTATED")

	if got := b.Channel("a")[0].Content.Text(); got != "alpha" {
		t.Errorf("ChannelsCopy is not deep; live board mutated: %q", got)
	}
	if _, ok := cp["b"]; !ok {
		t.Error("ChannelsCopy must include every channel")
	}
}

func TestBoard_Clone_DeepCopiesMessageParts(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage(agent.MainChannel, message.Message{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{message.TextPart{Text: "original"}}},
	})

	cloned := b.Clone()
	if cloned == nil || cloned == b {
		t.Fatal("Clone returned nil or the same board")
	}

	// Mutating the original message part must not leak into the clone.
	original := b.Channel(agent.MainChannel)
	original[0].Content.Parts[0] = message.TextPart{Text: "mutated"}
	b.SetChannel(agent.MainChannel, original)

	got := cloned.Channel(agent.MainChannel)
	if len(got) != 1 {
		t.Fatalf("clone channel length = %d, want 1", len(got))
	}
	text, ok := got[0].Content.Parts[0].(message.TextPart)
	if !ok || text.Text != "original" {
		t.Fatalf("clone part = %#v, want original text", got[0].Content.Parts[0])
	}
}

func TestBoard_SnapshotRestoreRoundTrip(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("k", "v")
	b.AppendChannelMessage("a", message.NewTextMessage(message.RoleUser, "alpha"))

	snap := b.Snapshot()

	b.SetVar("k", "MUTATED")
	b.AppendChannelMessage("a", message.NewTextMessage(message.RoleUser, "leaked"))

	if v := snap.Vars["k"]; v != "v" {
		t.Errorf("snapshot Vars leaked mutation: %v", v)
	}
	if got := snap.Channels["a"]; len(got) != 1 || got[0].Content.Text() != "alpha" {
		t.Errorf("snapshot Channels leaked mutation: %+v", got)
	}

	restored := agent.RestoreBoard(snap)
	if v, _ := restored.GetVar("k"); v != "v" {
		t.Errorf("RestoreBoard k = %v, want v", v)
	}
	if got := restored.Channel("a"); len(got) != 1 || got[0].Content.Text() != "alpha" {
		t.Errorf("RestoreBoard channel a = %+v, want [alpha]", got)
	}
}

func TestBoard_Snapshot_DeepCopiesChannelMessageParts(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage("a", message.Message{
		Role: message.RoleUser,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "alpha"},
			imageURLPart(t, "https://img.example.com/a.png"),
		}},
	})

	snap := b.Snapshot()
	got := b.Channel("a")
	got[0].Content.Parts[0] = message.TextPart{Text: "MUTATED"}
	got[0].Content.Parts[1] = imageURLPart(t, "https://img.example.com/MUTATED.png")

	if text := snap.Channels["a"][0].Content.Parts[0].(message.TextPart).Text; text != "alpha" {
		t.Errorf("snapshot channel part leaked mutation: %q", text)
	}
	if url := snap.Channels["a"][0].Content.Parts[1].(message.ImagePart).Source.URL(); url != "https://img.example.com/a.png" {
		t.Errorf("snapshot channel media leaked mutation: %q", url)
	}
}

func TestBoard_RestoreBoard_NilSnapshot(t *testing.T) {
	b := agent.RestoreBoard(nil)
	if b == nil {
		t.Fatal("RestoreBoard(nil) must return a fresh empty board, not nil")
	}
	if b.Channel(agent.MainChannel) != nil {
		t.Errorf("fresh board MainChannel should be empty")
	}
}

func TestBoard_RestoreFrom_EmptyChannelsRehydratesMain(t *testing.T) {
	b := agent.NewBoard()
	b.AppendChannelMessage("a", message.NewTextMessage(message.RoleUser, "x"))

	snap := &agent.BoardSnapshot{Vars: map[string]any{"k": "v"}}
	b.RestoreFrom(snap)

	if v, _ := b.GetVar("k"); v != "v" {
		t.Errorf("RestoreFrom did not import vars: got %v", v)
	}
	if got := b.Channel("a"); got != nil {
		t.Errorf("RestoreFrom did not clear stale channels; got %+v", got)
	}
}

func TestBoard_RestoreFrom_NilIsNoOp(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("k", "v")
	b.RestoreFrom(nil)
	if v, _ := b.GetVar("k"); v != "v" {
		t.Errorf("RestoreFrom(nil) should be a no-op; got %v", v)
	}
}

// cloneable verifies the Cloneable fast-path is preferred over
// reflection.
type cloneable struct {
	V int
}

func (c *cloneable) Clone() any {
	return &cloneable{V: c.V * 10}
}

func TestBoard_Snapshot_HonorsCloneable(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("c", &cloneable{V: 7})

	snap := b.Snapshot()
	got, ok := snap.Vars["c"].(*cloneable)
	if !ok {
		t.Fatalf("snapshot value type = %T, want *cloneable", snap.Vars["c"])
	}
	if got.V != 70 {
		t.Errorf("Cloneable.Clone not invoked; V=%d (want 70)", got.V)
	}
}

// reflectStruct exercises the reflection deep-copy fall-through for
// struct values that don't implement Cloneable and aren't a special-
// cased primitive / map / slice.
type reflectStruct struct {
	N int
	M map[string]int
	S []*reflectStruct
}

func TestBoard_Snapshot_ReflectionDeepCopiesStruct(t *testing.T) {
	original := &reflectStruct{
		N: 1,
		M: map[string]int{"a": 1},
		S: []*reflectStruct{{N: 2}},
	}

	b := agent.NewBoard()
	b.SetVar("rs", original)

	snap := b.Snapshot()

	// Mutate live value; snapshot copy must be unaffected.
	original.N = 99
	original.M["a"] = 99
	original.S[0].N = 99

	got, ok := snap.Vars["rs"].(*reflectStruct)
	if !ok {
		t.Fatalf("snapshot type = %T, want *reflectStruct", snap.Vars["rs"])
	}
	if got.N != 1 {
		t.Errorf("snapshot N = %d, want 1 (deep copy missed top-level)", got.N)
	}
	if got.M["a"] != 1 {
		t.Errorf("snapshot M[a] = %d, want 1 (deep copy missed map)", got.M["a"])
	}
	if got.S[0].N != 2 {
		t.Errorf("snapshot S[0].N = %d, want 2 (deep copy missed nested pointer)", got.S[0].N)
	}
}

func TestBoard_Snapshot_PreservesPrimitives(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("i", 42)
	b.SetVar("s", "v")
	b.SetVar("f", 3.14)
	b.SetVar("bv", true)

	snap := b.Snapshot()

	cases := map[string]any{
		"i": 42, "s": "v", "f": 3.14, "bv": true,
	}
	for k, want := range cases {
		if got := snap.Vars[k]; got != want {
			t.Errorf("snap.Vars[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestBoard_Snapshot_PreservesStructUnexportedFields(t *testing.T) {
	ts := time.Date(2026, 4, 27, 17, 53, 0, 123, time.FixedZone("CST", 8*60*60))
	b := agent.NewBoard()
	b.SetVar("ts", ts)

	snap := b.Snapshot()
	got, ok := snap.Vars["ts"].(time.Time)
	if !ok {
		t.Fatalf("snapshot value type = %T, want time.Time", snap.Vars["ts"])
	}
	if !got.Equal(ts) {
		t.Fatalf("snapshot time = %v, want %v", got, ts)
	}
	if got.Location().String() != ts.Location().String() {
		t.Fatalf("snapshot time location = %v, want %v", got.Location(), ts.Location())
	}
}

func TestBoard_Snapshot_HandlesNestedSliceOfAny(t *testing.T) {
	b := agent.NewBoard()
	b.SetVar("xs", []any{1, "two", true, []any{3, 4}})

	snap := b.Snapshot()

	live := []any{1, "two", true, []any{3, 4}}
	live[0] = 999

	got := snap.Vars["xs"].([]any)
	if got[0] != 1 {
		t.Errorf("nested []any not deep-copied; got[0] = %v", got[0])
	}
	if inner := got[3].([]any); inner[0] != 3 {
		t.Errorf("inner []any not deep-copied; inner[0] = %v", inner[0])
	}
}

func TestBoard_Snapshot_NilPointerHandled(t *testing.T) {
	var p *reflectStruct
	b := agent.NewBoard()
	b.SetVar("nilp", p)

	snap := b.Snapshot() // must not panic
	if got := snap.Vars["nilp"]; got != nil {
		// Reflect path returns a typed-nil; equality check uses
		// the typed comparison so we accept either form.
		if rp, ok := got.(*reflectStruct); !ok || rp != nil {
			t.Errorf("nilp snapshot = %T:%v, want nil pointer", got, got)
		}
	}
}

func TestBoard_ConcurrentAccessRaceSmoke(t *testing.T) {
	b := agent.NewBoard()

	var counter int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.AppendChannelMessage(agent.MainChannel,
				message.NewTextMessage(message.RoleAssistant, "x"))
			atomic.AddInt64(&counter, 1)
		}()
		go func() {
			defer wg.Done()
			_ = b.Channel(agent.MainChannel)
		}()
	}
	wg.Wait()

	if got := b.Channel(agent.MainChannel); int64(len(got)) != atomic.LoadInt64(&counter) {
		t.Errorf("appended messages = %d, recorded counter = %d",
			len(got), atomic.LoadInt64(&counter))
	}
}
