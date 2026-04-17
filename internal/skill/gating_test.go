package skill

import (
	"runtime"
	"testing"
)

func TestEvaluateGating_NilRequires(t *testing.T) {
	meta := &SkillMeta{Name: "test"}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("nil requires should be available")
	}
}

func TestEvaluateGating_OSMatch(t *testing.T) {
	meta := &SkillMeta{
		Name:     "current-os",
		Requires: &SkillRequires{OS: []string{runtime.GOOS}},
	}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("should be available when OS matches")
	}
	if g.Reason != "" {
		t.Fatalf("unexpected reason: %q", g.Reason)
	}
}

func TestEvaluateGating_OSMismatch(t *testing.T) {
	meta := &SkillMeta{
		Name:     "win-only",
		Requires: &SkillRequires{OS: []string{"windows"}},
	}
	if runtime.GOOS == "windows" {
		t.Skip("test requires non-windows OS")
	}
	g := evaluateGating(meta)
	if g.Available {
		t.Fatal("should be unavailable on non-windows")
	}
	if g.Reason == "" {
		t.Fatal("should have reason for OS mismatch")
	}
}

func TestEvaluateGating_OSCaseInsensitive(t *testing.T) {
	upper := "DARWIN"
	if runtime.GOOS != "darwin" {
		upper = "LINUX"
		if runtime.GOOS != "linux" {
			t.Skipf("test requires darwin or linux, got %s", runtime.GOOS)
		}
	}
	meta := &SkillMeta{
		Name:     "case-test",
		Requires: &SkillRequires{OS: []string{upper}},
	}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("OS check should be case-insensitive")
	}
}

func TestEvaluateGating_BinPresent(t *testing.T) {
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{Bins: []string{"sh"}},
	}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("'sh' should be available on all unix systems")
	}
	if len(g.MissingBins) != 0 {
		t.Fatalf("unexpected missing bins: %v", g.MissingBins)
	}
}

func TestEvaluateGating_MissingBin(t *testing.T) {
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{Bins: []string{"__nonexistent_binary_xyz__"}},
	}
	g := evaluateGating(meta)
	if g.Available {
		t.Fatal("should be unavailable with missing bin")
	}
	if len(g.MissingBins) != 1 {
		t.Fatalf("expected 1 missing bin, got %d", len(g.MissingBins))
	}
}

func TestEvaluateGating_MultipleBins_PartialMissing(t *testing.T) {
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{Bins: []string{"sh", "__nonexistent__"}},
	}
	g := evaluateGating(meta)
	if g.Available {
		t.Fatal("should be unavailable when any required bin is missing")
	}
	if len(g.MissingBins) != 1 || g.MissingBins[0] != "__nonexistent__" {
		t.Fatalf("only the missing bin should be reported: %v", g.MissingBins)
	}
}

func TestEvaluateGating_AnyBinsOneSatisfied(t *testing.T) {
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{AnyBins: []string{"__nonexistent__", "sh"}},
	}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("should be available when at least one anyBin exists")
	}
}

func TestEvaluateGating_AnyBinsAllMissing(t *testing.T) {
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{AnyBins: []string{"__nonexistent1__", "__nonexistent2__"}},
	}
	g := evaluateGating(meta)
	if g.Available {
		t.Fatal("should be unavailable when all anyBins missing")
	}
	if len(g.MissingAnyBins) != 2 {
		t.Fatalf("expected 2 missing_any_bins, got %d", len(g.MissingAnyBins))
	}
	if len(g.MissingBins) != 0 {
		t.Fatalf("MissingBins should be empty for AnyBins failure, got %v", g.MissingBins)
	}
}

func TestEvaluateGating_MissingEnv(t *testing.T) {
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{Env: []string{"__NONEXISTENT_ENV_VAR_XYZ__"}},
	}
	g := evaluateGating(meta)
	if g.Available {
		t.Fatal("should be unavailable with missing env")
	}
	if len(g.MissingEnv) != 1 {
		t.Fatalf("expected 1 missing env, got %d", len(g.MissingEnv))
	}
}

func TestEvaluateGating_EmptyEnvIsPresent(t *testing.T) {
	t.Setenv("__GATING_TEST_EMPTY__", "")
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{Env: []string{"__GATING_TEST_EMPTY__"}},
	}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("env set to empty string should be treated as present")
	}
}

func TestEvaluateGating_EnvPresent(t *testing.T) {
	t.Setenv("__GATING_TEST_VAR__", "value")
	meta := &SkillMeta{
		Name:     "test",
		Requires: &SkillRequires{Env: []string{"__GATING_TEST_VAR__"}},
	}
	g := evaluateGating(meta)
	if !g.Available {
		t.Fatal("env with value should be available")
	}
}

func TestEvaluateGating_Combined(t *testing.T) {
	meta := &SkillMeta{
		Name: "combo",
		Requires: &SkillRequires{
			Bins: []string{"__missing_bin__"},
			Env:  []string{"__MISSING_ENV__"},
		},
	}
	g := evaluateGating(meta)
	if g.Available {
		t.Fatal("should be unavailable with combined missing deps")
	}
	if len(g.MissingBins) != 1 {
		t.Fatalf("expected 1 missing bin, got %d", len(g.MissingBins))
	}
	if len(g.MissingEnv) != 1 {
		t.Fatalf("expected 1 missing env, got %d", len(g.MissingEnv))
	}
}

func TestGatingDeps_Available(t *testing.T) {
	g := &SkillGating{Available: true}
	deps := gatingDeps(g)
	if len(deps) != 0 {
		t.Fatalf("available skill should have no deps, got %v", deps)
	}
}

func TestGatingDeps_Nil(t *testing.T) {
	deps := gatingDeps(nil)
	if len(deps) != 0 {
		t.Fatalf("nil gating should have no deps, got %v", deps)
	}
}

func TestGatingDeps_MissingAll(t *testing.T) {
	g := &SkillGating{
		Available:      false,
		MissingBins:    []string{"curl"},
		MissingAnyBins: []string{"xurl", "wget"},
		MissingEnv:     []string{"API_KEY"},
	}
	deps := gatingDeps(g)
	if len(deps) != 3 {
		t.Fatalf("expected 3 deps, got %d: %v", len(deps), deps)
	}
	if deps[0] != "curl" {
		t.Fatalf("first dep should be curl, got %q", deps[0])
	}
	if deps[1] != "one of: xurl|wget" {
		t.Fatalf("second dep should be 'one of: xurl|wget', got %q", deps[1])
	}
	if deps[2] != "API_KEY" {
		t.Fatalf("third dep should be API_KEY, got %q", deps[2])
	}
}

func TestFormatGatingMessage_Available(t *testing.T) {
	meta := &SkillMeta{Gating: &SkillGating{Available: true}}
	if msg := formatGatingMessage(meta); msg != "" {
		t.Fatalf("expected empty, got %q", msg)
	}
}

func TestFormatGatingMessage_NilGating(t *testing.T) {
	meta := &SkillMeta{}
	if msg := formatGatingMessage(meta); msg != "" {
		t.Fatalf("expected empty, got %q", msg)
	}
}

func TestFormatGatingMessage_Unavailable(t *testing.T) {
	meta := &SkillMeta{
		Gating: &SkillGating{
			Available:      false,
			MissingBins:    []string{"curl"},
			MissingAnyBins: []string{"a", "b"},
			MissingEnv:     []string{"KEY"},
		},
	}
	msg := formatGatingMessage(meta)
	if msg == "" {
		t.Fatal("expected non-empty message")
	}
	if !contains(msg, "missing binaries: curl") {
		t.Fatalf("expected missing binaries, got %q", msg)
	}
	if !contains(msg, "need one of: a, b") {
		t.Fatalf("expected need one of, got %q", msg)
	}
	if !contains(msg, "missing env vars: KEY") {
		t.Fatalf("expected missing env vars, got %q", msg)
	}
}

func TestFormatGatingMessage_WithReason(t *testing.T) {
	meta := &SkillMeta{
		Gating: &SkillGating{
			Available: false,
			Reason:    "unsupported OS: windows",
		},
	}
	msg := formatGatingMessage(meta)
	if msg != "unsupported OS: windows" {
		t.Fatalf("expected Reason to be used, got %q", msg)
	}
}
