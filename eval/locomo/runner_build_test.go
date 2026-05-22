package locomo

import "testing"

func TestNormalizeRunnerName_UsesRecallNamesWithLegacyAliases(t *testing.T) {
	tests := map[string]string{
		"flowcraft":           runnerFlowcraftRecallV1,
		"flowcraft-v1":        runnerFlowcraftRecallV1,
		"flowcraft-v2":        runnerFlowcraftRecallV2,
		"flowcraft-recall-v1": runnerFlowcraftRecallV1,
		"flowcraft-recall-v2": runnerFlowcraftRecallV2,
	}
	for in, want := range tests {
		got, err := normalizeRunnerName(in)
		if err != nil {
			t.Fatalf("normalizeRunnerName(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("normalizeRunnerName(%q) = %q, want %q", in, got, want)
		}
	}
}
