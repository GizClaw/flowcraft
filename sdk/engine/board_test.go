package engine_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestBoard_NewBoardHasEmptyMainChannel(t *testing.T) {
	b := engine.NewBoard()
	if got := b.Channel(engine.MainChannel); got != nil {
		t.Errorf("MainChannel = %+v, want nil for empty channel", got)
	}
}

func TestBoard_AppendChannelMessage(t *testing.T) {
	b := engine.NewBoard()
	m := model.NewTextMessage(model.RoleUser, "hello")
	b.AppendChannelMessage(engine.MainChannel, m)

	got := b.Channel(engine.MainChannel)
	if len(got) != 1 || got[0].Content() != "hello" {
		t.Errorf("Channel = %+v, want [hello]", got)
	}
}

func TestBoard_SetChannel_DefensiveCopy(t *testing.T) {
	b := engine.NewBoard()
	in := []model.Message{
		model.NewTextMessage(model.RoleUser, "a"),
		model.NewTextMessage(model.RoleUser, "b"),
	}
	b.SetChannel("alt", in)

	// Mutate caller-owned slice; board copy must be unaffected.
	in[0] = model.NewTextMessage(model.RoleUser, "MUTATED")

	got := b.Channel("alt")
	if got[0].Content() != "a" {
		t.Errorf("SetChannel did not defensively copy; got %q after caller mutation", got[0].Content())
	}
}

func TestBoard_SetChannel_DeepCopiesMessageParts(t *testing.T) {
	b := engine.NewBoard()
	in := []model.Message{
		{
			Role: model.RoleUser,
			Parts: []model.Part{
				{Type: model.PartText, Text: "a"},
				{Type: model.PartImage, Image: &model.MediaRef{URL: "https://img.example.com/a.png"}},
			},
		},
	}
	b.SetChannel("alt", in)

	in[0].Parts[0].Text = "MUTATED"
	in[0].Parts[1].Image.URL = "MUTATED"

	got := b.Channel("alt")
	if got[0].Parts[0].Text != "a" {
		t.Errorf("SetChannel leaked caller part mutation: %q", got[0].Parts[0].Text)
	}
	if got[0].Parts[1].Image.URL != "https://img.example.com/a.png" {
		t.Errorf("SetChannel leaked caller media mutation: %q", got[0].Parts[1].Image.URL)
	}
}

func TestBoard_ChannelDefensiveCopyOnRead(t *testing.T) {
	b := engine.NewBoard()
	b.AppendChannelMessage("alt", model.NewTextMessage(model.RoleUser, "x"))

	got := b.Channel("alt")
	got[0] = model.NewTextMessage(model.RoleUser, "MUTATED")

	again := b.Channel("alt")
	if again[0].Content() != "x" {
		t.Errorf("Channel returned a non-defensive view; mutation leaked: %q", again[0].Content())
	}
}

func TestBoard_ChannelDeepCopiesMessagePartsOnRead(t *testing.T) {
	b := engine.NewBoard()
	b.AppendChannelMessage("alt", model.Message{
		Role: model.RoleUser,
		Parts: []model.Part{
			{Type: model.PartText, Text: "x"},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://img.example.com/x.png"}},
		},
	})

	got := b.Channel("alt")
	got[0].Parts[0].Text = "MUTATED"
	got[0].Parts[1].Image.URL = "MUTATED"

	again := b.Channel("alt")
	if again[0].Parts[0].Text != "x" {
		t.Errorf("Channel leaked part mutation: %q", again[0].Parts[0].Text)
	}
	if again[0].Parts[1].Image.URL != "https://img.example.com/x.png" {
		t.Errorf("Channel leaked media mutation: %q", again[0].Parts[1].Image.URL)
	}
}

func TestBoard_VarsRoundTrip(t *testing.T) {
	b := engine.NewBoard()
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

	if v, ok := engine.GetTyped[int](b, "n"); !ok || v != 42 {
		t.Errorf("GetTyped[int] = (%v, %v)", v, ok)
	}
	if _, ok := engine.GetTyped[bool](b, "n"); ok {
		t.Error("GetTyped should fail when types disagree")
	}
}

func TestBoard_VarsReturnsSnapshot(t *testing.T) {
	b := engine.NewBoard()
	b.SetVar("k", "v")

	got := b.Vars()
	got["k"] = "MUTATED"

	if v, _ := b.GetVar("k"); v != "v" {
		t.Errorf("Board mutated through Vars copy: got %v", v)
	}
}

func TestBoard_AppendSliceVar_NewKey(t *testing.T) {
	b := engine.NewBoard()
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
	b := engine.NewBoard()
	b.SetVar("not_slice", "string")

	err := b.AppendSliceVar("not_slice", "x")
	if err == nil {
		t.Error("AppendSliceVar onto non-[]any value should return error")
	}
}

func TestBoard_UpdateSliceVarItem(t *testing.T) {
	b := engine.NewBoard()
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
	b := engine.NewBoard()
	b.UpdateSliceVarItem("missing",
		func(any) bool { return true },
		func(any) any { return "x" },
	)
	if v, ok := b.GetVar("missing"); ok {
		t.Errorf("UpdateSliceVarItem on missing key should not create the key; got %v", v)
	}
}

func TestBoard_ChannelsCopy_DeepCopiesPerChannel(t *testing.T) {
	b := engine.NewBoard()
	b.AppendChannelMessage("a", model.NewTextMessage(model.RoleUser, "alpha"))
	b.AppendChannelMessage("b", model.NewTextMessage(model.RoleUser, "beta"))

	cp := b.ChannelsCopy()
	cp["a"][0] = model.NewTextMessage(model.RoleUser, "MUTATED")

	if got := b.Channel("a")[0].Content(); got != "alpha" {
		t.Errorf("ChannelsCopy is not deep; live board mutated: %q", got)
	}
	if _, ok := cp["b"]; !ok {
		t.Error("ChannelsCopy must include every channel")
	}
}

func TestBoard_SnapshotRestoreRoundTrip(t *testing.T) {
	b := engine.NewBoard()
	b.SetVar("k", "v")
	b.AppendChannelMessage("a", model.NewTextMessage(model.RoleUser, "alpha"))

	snap := b.Snapshot()

	b.SetVar("k", "MUTATED")
	b.AppendChannelMessage("a", model.NewTextMessage(model.RoleUser, "leaked"))

	if v := snap.Vars["k"]; v != "v" {
		t.Errorf("snapshot Vars leaked mutation: %v", v)
	}
	if got := snap.Channels["a"]; len(got) != 1 || got[0].Content() != "alpha" {
		t.Errorf("snapshot Channels leaked mutation: %+v", got)
	}

	restored := engine.RestoreBoard(snap)
	if v, _ := restored.GetVar("k"); v != "v" {
		t.Errorf("RestoreBoard k = %v, want v", v)
	}
	if got := restored.Channel("a"); len(got) != 1 || got[0].Content() != "alpha" {
		t.Errorf("RestoreBoard channel a = %+v, want [alpha]", got)
	}
}

func TestBoard_Snapshot_DeepCopiesChannelMessageParts(t *testing.T) {
	b := engine.NewBoard()
	b.AppendChannelMessage("a", model.Message{
		Role: model.RoleUser,
		Parts: []model.Part{
			{Type: model.PartText, Text: "alpha"},
			{Type: model.PartImage, Image: &model.MediaRef{URL: "https://img.example.com/a.png"}},
		},
	})

	snap := b.Snapshot()
	got := b.Channel("a")
	got[0].Parts[0].Text = "MUTATED"
	got[0].Parts[1].Image.URL = "MUTATED"

	if snap.Channels["a"][0].Parts[0].Text != "alpha" {
		t.Errorf("snapshot channel part leaked mutation: %q", snap.Channels["a"][0].Parts[0].Text)
	}
	if snap.Channels["a"][0].Parts[1].Image.URL != "https://img.example.com/a.png" {
		t.Errorf("snapshot channel media leaked mutation: %q", snap.Channels["a"][0].Parts[1].Image.URL)
	}
}

func TestBoard_RestoreBoard_NilSnapshot(t *testing.T) {
	b := engine.RestoreBoard(nil)
	if b == nil {
		t.Fatal("RestoreBoard(nil) must return a fresh empty board, not nil")
	}
	if b.Channel(engine.MainChannel) != nil {
		t.Errorf("fresh board MainChannel should be empty")
	}
}

func TestBoard_RestoreFrom_EmptyChannelsRehydratesMain(t *testing.T) {
	b := engine.NewBoard()
	b.AppendChannelMessage("a", model.NewTextMessage(model.RoleUser, "x"))

	snap := &engine.BoardSnapshot{Vars: map[string]any{"k": "v"}}
	b.RestoreFrom(snap)

	if v, _ := b.GetVar("k"); v != "v" {
		t.Errorf("RestoreFrom did not import vars: got %v", v)
	}
	if got := b.Channel("a"); got != nil {
		t.Errorf("RestoreFrom did not clear stale channels; got %+v", got)
	}
}

func TestBoard_RestoreFrom_NilIsNoOp(t *testing.T) {
	b := engine.NewBoard()
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
	b := engine.NewBoard()
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

	b := engine.NewBoard()
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
	b := engine.NewBoard()
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
	b := engine.NewBoard()
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
	b := engine.NewBoard()
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
	b := engine.NewBoard()
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
	b := engine.NewBoard()

	var counter int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.AppendChannelMessage(engine.MainChannel,
				model.NewTextMessage(model.RoleAssistant, "x"))
			atomic.AddInt64(&counter, 1)
		}()
		go func() {
			defer wg.Done()
			_ = b.Channel(engine.MainChannel)
		}()
	}
	wg.Wait()

	if got := b.Channel(engine.MainChannel); int64(len(got)) != atomic.LoadInt64(&counter) {
		t.Errorf("appended messages = %d, recorded counter = %d",
			len(got), atomic.LoadInt64(&counter))
	}
}
