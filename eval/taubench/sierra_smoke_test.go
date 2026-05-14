package taubench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSierraRetailShadowRun_AllTasks loads the full 115-task
// retail test set produced by eval/taubench/sierra/prep.py and
// shadow-runs every task's gold action sequence. A task whose
// gold trace fails to execute against a fresh clone of the initial
// state is almost always a tool-port bug (missing tool, wrong
// kwargs handling, wrong state mutation) and is the cheapest gate
// against drift between Sierra's Python harness and our Go port.
//
// The test is gated on FLOWCRAFT_TAUBENCH_SIERRA_DATA pointing at a
// directory laid out as:
//
//	<root>/retail/initial_state.json
//	<root>/retail/tasks_test.json
//	<root>/airline/initial_state.json
//	<root>/airline/tasks_test.json
//
// (the canonical output of prep.py). Without the env var the test
// is skipped — Sierra data is not committed to the repo (210k lines
// of JSON, CC-BY) and is staged on the runner host.
func TestSierraRetailShadowRun_AllTasks(t *testing.T) {
	root := os.Getenv("FLOWCRAFT_TAUBENCH_SIERRA_DATA")
	if root == "" {
		t.Skip("FLOWCRAFT_TAUBENCH_SIERRA_DATA not set; stage Sierra data and re-run")
	}
	t.Run("retail", func(t *testing.T) {
		runShadowSmoke(t, root, "retail", NewSierraRetailTools())
	})
	t.Run("airline", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(root, "airline", "tasks_test.json")); err != nil {
			t.Skip("airline tools not yet ported")
		}
		runShadowSmoke(t, root, "airline", NewSierraAirlineTools())
	})
}

func runShadowSmoke(t *testing.T, root, domain string, tools map[string]Tool) {
	t.Helper()
	stateRaw, err := os.ReadFile(filepath.Join(root, domain, "initial_state.json"))
	if err != nil {
		t.Fatalf("read initial state: %v", err)
	}
	var initial State
	if err := json.Unmarshal(stateRaw, &initial); err != nil {
		t.Fatalf("parse initial state: %v", err)
	}
	tasks, err := LoadUpstreamTasks(filepath.Join(root, domain, "tasks_test.json"), initial, tools, domain)
	if err != nil {
		t.Fatalf("load + shadow run: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatalf("expected > 0 tasks, got 0")
	}
	for _, task := range tasks {
		if len(task.Expected.ExpectedFinalState) == 0 && len(task.Expected.ExpectedTextFragments) == 0 {
			t.Errorf("task %s: ExpectedFinalState and ExpectedTextFragments both empty (shadow run produced no signal)", task.ID)
		}
	}
	t.Logf("%s: shadow-ran %d tasks, no errors", domain, len(tasks))
}
