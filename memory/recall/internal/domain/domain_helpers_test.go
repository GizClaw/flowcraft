package domain

import (
	"testing"
	"time"
)

// TestNormalizeForgetMode pins the default: empty / unknown inputs
// map to ForgetHard so callers that omit the parameter (or send a
// stale string) get the same behaviour as the historical Forget API.
func TestNormalizeForgetMode(t *testing.T) {
	cases := []struct {
		in   ForgetMode
		want ForgetMode
	}{
		{"", ForgetHard},
		{ForgetSoft, ForgetSoft},
		{ForgetHard, ForgetHard},
		{"unknown", ForgetHard},
	}
	for _, tc := range cases {
		if got := NormalizeForgetMode(tc.in); got != tc.want {
			t.Errorf("NormalizeForgetMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFactKind_IsValid(t *testing.T) {
	valid := []FactKind{KindEvent, KindState, KindPreference, KindProcedure, KindRelation, KindPlan, KindNote, KindParameter}
	for _, k := range valid {
		if !k.IsValid() {
			t.Errorf("%q must be valid", k)
		}
	}
	for _, k := range []FactKind{"", "unknown", "fact"} {
		if k.IsValid() {
			t.Errorf("%q must be invalid", k)
		}
	}
}

func TestTimeRange(t *testing.T) {
	if !(TimeRange{}).IsZero() {
		t.Error("zero range is zero")
	}
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if (TimeRange{From: t1}).IsZero() {
		t.Error("range with From set is not zero")
	}
	if (TimeRange{To: t1}).IsZero() {
		t.Error("range with To set is not zero")
	}
	r := TimeRangeFrom(t1, t1.Add(time.Hour))
	if !r.From.Equal(t1) || !r.To.Equal(t1.Add(time.Hour)) {
		t.Errorf("TimeRangeFrom = %+v", r)
	}
}

func TestScopesMatch(t *testing.T) {
	a := Scope{RuntimeID: "rt", UserID: "alice"}
	b := Scope{RuntimeID: "rt", UserID: "alice", Federation: []Scope{{RuntimeID: "rt"}}}
	c := Scope{RuntimeID: "rt", UserID: "bob"}
	if !ScopesMatch(a, b) {
		t.Error("federation is recall-only; primary keys must still match")
	}
	if ScopesMatch(a, c) {
		t.Error("different UserID must not match")
	}
	agentA := Scope{RuntimeID: "rt", UserID: "alice", AgentID: "a1"}
	agentB := Scope{RuntimeID: "rt", UserID: "alice", AgentID: "a2"}
	if !ScopesMatch(agentA, agentB) {
		t.Error("AgentID must not affect store partition match")
	}
}

func TestNormalizeSaveTier(t *testing.T) {
	cases := map[string]string{
		"":          TierGeneral,
		"unknown":   TierGeneral,
		TierCore:    TierCore,
		TierData:    TierData,
		TierStorage: TierStorage,
	}
	for in, want := range cases {
		if got := NormalizeSaveTier(in); got != want {
			t.Errorf("NormalizeSaveTier(%q) = %q, want %q", in, got, want)
		}
	}
}
