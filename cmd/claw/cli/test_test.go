package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadTestEmbeddedByCasePath(t *testing.T) {
	name, test, err := readTest("match_route/music_flow")
	if err != nil {
		t.Fatalf("readTest: %v", err)
	}
	if name != "music_flow" {
		t.Fatalf("name = %q, want music_flow", name)
	}
	if test.Raid != "match_route" {
		t.Fatalf("raid = %q, want match_route", test.Raid)
	}
	if len(test.Turns) != 2 || test.Turns[0] != "我要听歌" || test.Turns[1] != "卡农" {
		t.Fatalf("turns = %#v", test.Turns)
	}
}

func TestReadTestEmbeddedByTestPath(t *testing.T) {
	name, test, err := readTest("test/match_route/music_flow")
	if err != nil {
		t.Fatalf("readTest test path: %v", err)
	}
	if name != "music_flow" || test.Raid != "match_route" {
		t.Fatalf("name=%q raid=%q", name, test.Raid)
	}
}

func TestReadTestEmbeddedByLegacyTestsPath(t *testing.T) {
	name, test, err := readTest("tests/match_route/music_flow")
	if err != nil {
		t.Fatalf("readTest legacy tests path: %v", err)
	}
	if name != "music_flow" || test.Raid != "match_route" {
		t.Fatalf("name=%q raid=%q", name, test.Raid)
	}
}

func TestReadTestRejectsBareEmbeddedCaseName(t *testing.T) {
	if _, _, err := readTest("music_flow"); err == nil {
		t.Fatal("expected bare embedded case name to fail")
	}
}

func TestReadTestFallsBackToLocalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local.yaml")
	if err := os.WriteFile(path, []byte("name: local\nraid: chat\nturns:\n  - hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	name, test, err := readTest(path)
	if err != nil {
		t.Fatalf("readTest local: %v", err)
	}
	if name != "local" || test.Raid != "chat" || len(test.Turns) != 1 {
		t.Fatalf("name=%q test=%+v", name, test)
	}
}

func TestPrepareTestRunDirectoryName(t *testing.T) {
	runDir := testRunDir("journey", time.Date(2026, 6, 5, 12, 45, 1, 0, time.Local))
	want := filepath.Join(".out", "journey_-_20260605_124501")
	if runDir != want {
		t.Fatalf("runDir = %q, want %q", runDir, want)
	}
}
