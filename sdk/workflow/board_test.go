package workflow

import (
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// --- Cloneable test helper ---

type cloneableVal struct {
	N int
}

func (c cloneableVal) Clone() any {
	return cloneableVal{N: c.N}
}

// --- Vars ---

func TestBoard_SetGetVar(t *testing.T) {
	b := NewBoard()
	b.SetVar("k", 42)

	v, ok := b.GetVar("k")
	if !ok || v != 42 {
		t.Fatalf("expected 42, got %v ok=%v", v, ok)
	}

	_, ok = b.GetVar("missing")
	if ok {
		t.Fatal("expected missing key to return false")
	}
}

func TestBoard_GetVarString(t *testing.T) {
	b := NewBoard()
	b.SetVar("s", "hello")
	b.SetVar("n", 123)

	if s := b.GetVarString("s"); s != "hello" {
		t.Fatalf("expected 'hello', got %q", s)
	}
	if s := b.GetVarString("n"); s != "" {
		t.Fatalf("expected empty for int, got %q", s)
	}
	if s := b.GetVarString("missing"); s != "" {
		t.Fatalf("expected empty for missing, got %q", s)
	}
}

func TestBoard_GetTyped(t *testing.T) {
	b := NewBoard()
	b.SetVar("n", 42)

	n, ok := GetTyped[int](b, "n")
	if !ok || n != 42 {
		t.Fatalf("expected 42, got %v ok=%v", n, ok)
	}

	_, ok = GetTyped[string](b, "n")
	if ok {
		t.Fatal("expected type mismatch")
	}

	_, ok = GetTyped[int](b, "missing")
	if ok {
		t.Fatal("expected missing key")
	}
}

func TestBoard_Vars(t *testing.T) {
	b := NewBoard()
	b.SetVar("a", 1)
	b.SetVar("b", "x")

	vars := b.Vars()
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(vars))
	}

	vars["c"] = "injected"
	if _, ok := b.GetVar("c"); ok {
		t.Fatal("Vars() should return a copy, not a reference")
	}
}

// --- AppendSliceVar ---

func TestBoard_AppendSliceVar(t *testing.T) {
	b := NewBoard()

	if err := b.AppendSliceVar("items", "a"); err != nil {
		t.Fatal(err)
	}
	if err := b.AppendSliceVar("items", "b"); err != nil {
		t.Fatal(err)
	}

	v, ok := b.GetVar("items")
	if !ok {
		t.Fatal("expected items var")
	}
	slice := v.([]any)
	if len(slice) != 2 || slice[0] != "a" || slice[1] != "b" {
		t.Fatalf("unexpected: %v", slice)
	}
}

func TestBoard_AppendSliceVar_ErrorOnTypeMismatch(t *testing.T) {
	b := NewBoard()
	b.SetVar("val", "not a slice")

	err := b.AppendSliceVar("val", "new")
	if err == nil {
		t.Fatal("expected error when appending to non-slice var")
	}

	v, _ := b.GetVar("val")
	if v != "not a slice" {
		t.Fatalf("original value should be preserved, got %v", v)
	}
}

// --- UpdateSliceVarItem ---

func TestBoard_UpdateSliceVarItem(t *testing.T) {
	b := NewBoard()
	_ = b.AppendSliceVar("items", map[string]any{"id": "a", "v": 1})
	_ = b.AppendSliceVar("items", map[string]any{"id": "b", "v": 2})

	b.UpdateSliceVarItem("items", func(item any) bool {
		m := item.(map[string]any)
		return m["id"] == "a"
	}, func(item any) any {
		m := item.(map[string]any)
		m["v"] = 99
		return m
	})

	v, _ := b.GetVar("items")
	first := v.([]any)[0].(map[string]any)
	if first["v"] != 99 {
		t.Fatalf("expected updated value 99, got %v", first["v"])
	}
}

func TestBoard_UpdateSliceVarItem_NonSlice(t *testing.T) {
	b := NewBoard()
	b.SetVar("x", "not slice")
	b.UpdateSliceVarItem("x", func(any) bool { return true }, func(item any) any {
		t.Fatal("should not be called")
		return item
	})
}

// --- Channels ---

func TestBoard_ChannelsCopy(t *testing.T) {
	b := NewBoard()
	b.AppendChannelMessage("ch1", model.NewTextMessage(model.RoleUser, "hi"))
	b.AppendChannelMessage("ch2", model.NewTextMessage(model.RoleAssistant, "ok"))

	cp := b.ChannelsCopy()
	if len(cp) < 2 {
		t.Fatalf("expected at least 2 channels, got %d", len(cp))
	}
	if len(cp["ch1"]) != 1 || cp["ch1"][0].Content() != "hi" {
		t.Fatalf("ch1 mismatch: %v", cp["ch1"])
	}

	cp["ch1"][0] = model.NewTextMessage(model.RoleUser, "mutated")
	orig := b.Channel("ch1")
	if orig[0].Content() == "mutated" {
		t.Fatal("ChannelsCopy should return a deep copy")
	}
}

func TestBoard_Channel_Empty(t *testing.T) {
	b := NewBoard()
	msgs := b.Channel("nonexistent")
	if msgs != nil {
		t.Fatalf("expected nil for empty channel, got %v", msgs)
	}
}

// --- Snapshot / Restore ---

func TestBoard_Snapshot_Restore_WithChannels(t *testing.T) {
	b := NewBoard()
	b.SetVar("x", 10)
	b.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleUser, "hello"))

	snap := b.Snapshot()

	b.SetVar("x", 999)
	b.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleUser, "world"))

	restored := RestoreBoard(snap)
	v, _ := restored.GetVar("x")
	if v != 10 {
		t.Fatalf("expected 10, got %v", v)
	}
	msgs := restored.Channel(MainChannel)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestRestoreBoard_Nil(t *testing.T) {
	b := RestoreBoard(nil)
	if b == nil {
		t.Fatal("expected non-nil board from nil snapshot")
	}
	msgs := b.Channel(MainChannel)
	if msgs != nil {
		t.Fatalf("expected empty main channel, got %v", msgs)
	}
}

func TestBoard_RestoreFrom_Nil(t *testing.T) {
	b := NewBoard()
	b.SetVar("x", 1)
	b.RestoreFrom(nil)
	v, ok := b.GetVar("x")
	if !ok || v != 1 {
		t.Fatal("RestoreFrom(nil) should be a no-op")
	}
}

func TestBoard_RestoreFrom_WithChannels(t *testing.T) {
	b := NewBoard()
	b.SetVar("x", 10)
	b.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleUser, "hello"))
	snap := b.Snapshot()

	b.SetVar("x", 999)
	_ = b.AppendSliceVar("extra", 1)
	b.RestoreFrom(snap)

	v, _ := b.GetVar("x")
	if v != 10 {
		t.Fatalf("expected 10, got %v", v)
	}
	_, ok := b.GetVar("extra")
	if ok {
		t.Fatal("RestoreFrom should have removed extra var")
	}
}

func TestRestoreBoard_NoChannels_InitializesMainChannel(t *testing.T) {
	snap := &BoardSnapshot{
		Vars:     map[string]any{"k": "v"},
		Channels: nil,
	}
	b := RestoreBoard(snap)
	msgs := b.Channel(MainChannel)
	if msgs != nil {
		t.Fatalf("expected empty main channel, got %v", msgs)
	}
}

// TestBoard_RestoreFrom_NoChannels_InitializesMainChannel mirrors
// TestRestoreBoard_NoChannels_InitializesMainChannel for the in-place
// rollback path: RestoreFrom must guarantee MainChannel exists even
// when the snapshot's Channels map is nil/empty (every Board invariant
// — including the one NewBoard / RestoreBoard already uphold).
func TestBoard_RestoreFrom_NoChannels_InitializesMainChannel(t *testing.T) {
	b := NewBoard()
	b.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleUser, "before"))

	snap := &BoardSnapshot{
		Vars:     map[string]any{"k": "v"},
		Channels: nil,
	}
	b.RestoreFrom(snap)

	if _, ok := b.channels[MainChannel]; !ok {
		t.Fatal("RestoreFrom must initialize MainChannel when snapshot carries no channels")
	}
	if got := b.Channel(MainChannel); len(got) != 0 {
		t.Fatalf("expected empty MainChannel after restore, got %v", got)
	}
}

// --- deepCopyValue ---

func TestDeepCopyValue_Nil(t *testing.T) {
	if deepCopyValue(nil) != nil {
		t.Fatal("expected nil")
	}
}

func TestDeepCopyValue_Primitives(t *testing.T) {
	cases := []any{
		"hello", 42, int8(1), int16(2), int32(3), int64(4),
		uint(5), uint8(6), uint16(7), uint32(8), uint64(9),
		float32(1.0), float64(2.0), true,
	}
	for _, c := range cases {
		cp := deepCopyValue(c)
		if cp != c {
			t.Fatalf("primitive copy failed for %T: %v != %v", c, cp, c)
		}
	}
}

func TestDeepCopyValue_Messages(t *testing.T) {
	msgs := []model.Message{model.NewTextMessage(model.RoleUser, "hi")}
	cp := deepCopyValue(msgs).([]model.Message)
	if len(cp) != 1 || cp[0].Content() != "hi" {
		t.Fatalf("message copy failed: %v", cp)
	}
	cp[0] = model.NewTextMessage(model.RoleUser, "mutated")
	if msgs[0].Content() == "mutated" {
		t.Fatal("should be a deep copy")
	}
}

func TestDeepCopyValue_SliceAny(t *testing.T) {
	src := []any{"a", 42, []any{"nested"}}
	cp := deepCopyValue(src).([]any)
	if len(cp) != 3 {
		t.Fatalf("expected 3 items, got %d", len(cp))
	}

	nested := cp[2].([]any)
	nested[0] = "mutated"
	if src[2].([]any)[0] == "mutated" {
		t.Fatal("should be a deep copy")
	}
}

func TestDeepCopyValue_MapStringAny(t *testing.T) {
	src := map[string]any{
		"a": 1,
		"b": map[string]any{"nested": true},
	}
	cp := deepCopyValue(src).(map[string]any)
	if cp["a"] != 1 {
		t.Fatal("top-level value mismatch")
	}

	nested := cp["b"].(map[string]any)
	nested["nested"] = false
	if src["b"].(map[string]any)["nested"] != true {
		t.Fatal("should be a deep copy")
	}
}

func TestDeepCopyValue_Cloneable(t *testing.T) {
	src := cloneableVal{N: 42}
	cp := deepCopyValue(src).(cloneableVal)
	if cp.N != 42 {
		t.Fatalf("expected 42, got %d", cp.N)
	}
}

func TestDeepCopyValue_ReflectFallback(t *testing.T) {
	type custom struct{ Data []int }
	orig := custom{Data: []int{1, 2, 3}}
	copied := deepCopyValue(orig).(custom)

	copied.Data[0] = 99
	if orig.Data[0] != 1 {
		t.Fatal("reflect deep copy did not isolate slice")
	}
}

func TestDeepCopyVars_Nil(t *testing.T) {
	cp := deepCopyVars(nil)
	if cp == nil {
		t.Fatal("expected non-nil map")
	}
	if len(cp) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(cp))
	}
}

// --- concurrency ---

func TestBoard_ConcurrentAppendSliceVar(t *testing.T) {
	b := NewBoard()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(val int) {
			defer wg.Done()
			_ = b.AppendSliceVar("items", val)
		}(i)
	}
	wg.Wait()

	v, _ := b.GetVar("items")
	slice := v.([]any)
	if len(slice) != 50 {
		t.Fatalf("expected 50 items, got %d", len(slice))
	}
}
