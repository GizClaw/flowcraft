package dynamic

import "testing"

func TestState_SelectedExpiresAfterM(t *testing.T) {
	st := newState()
	st.selectNames([]string{"x"}, 2)
	if st.selected["x"] != 2 {
		t.Fatalf("selected rounds = %d, want 2", st.selected["x"])
	}
	st.advanceTurn(10)
	if st.selected["x"] != 1 {
		t.Fatalf("after one turn rounds = %d, want 1", st.selected["x"])
	}
	st.advanceTurn(10)
	if _, ok := st.selected["x"]; ok {
		t.Fatal("selected tool survived its retention window")
	}
}

func TestState_RecordCallRefreshesAndMarksRecent(t *testing.T) {
	st := newState()
	st.turn = 5
	st.recordCall("x", 3)
	if st.selected["x"] != 3 {
		t.Errorf("selected = %d, want 3", st.selected["x"])
	}
	if st.recent["x"] != 5 {
		t.Errorf("recent = %d, want 5", st.recent["x"])
	}
}

func TestState_RecentWindowExpires(t *testing.T) {
	st := newState()
	st.turn = 1
	st.recordCall("x", 1)
	for i := 0; i < 11; i++ {
		st.advanceTurn(5)
	}
	if _, ok := st.recent["x"]; ok {
		t.Error("recent entry survived the window")
	}
}
