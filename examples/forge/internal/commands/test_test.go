package commands

import (
	"strings"
	"testing"
	"time"
)

func TestParseTestFileAcceptsScenarioYAML(t *testing.T) {
	raw := []byte(`
name: companion_opening
description: Starts the companion and asks for music.
raid: companion
turns:
  - 帮我放一首轻音乐
  - 声音小一点
`)
	test, err := parseTestFile(raw)
	if err != nil {
		t.Fatalf("parseTestFile: %v", err)
	}
	if test.Name != "companion_opening" || test.Raid != "companion" {
		t.Fatalf("test = %+v", test)
	}
	if len(test.Turns) != 2 || test.Turns[1] != "声音小一点" {
		t.Fatalf("turns = %v", test.Turns)
	}
}

func TestParseTestFileRejectsWrongShape(t *testing.T) {
	_, err := parseTestFile([]byte("turns: not-a-list\n"))
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if !strings.Contains(err.Error(), "turns") {
		t.Fatalf("error %v does not name the field", err)
	}
}

func TestParseTestFileToleratesExtraFields(t *testing.T) {
	// Scenario tests are authoring files, not the deployment's strict contract:
	// a note beside the turns must not fail the decode.
	raw := []byte("name: x\nraid: y\nturns: [a]\nnotes: free text\n")
	if _, err := parseTestFile(raw); err != nil {
		t.Fatalf("extra fields must be tolerated: %v", err)
	}
}

func TestTestRunDirIsTimestamped(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	dir := testRunDir("companion/opening", now)
	if dir != ".out/companion_opening_20260102_030405" {
		t.Fatalf("dir = %q", dir)
	}
}
