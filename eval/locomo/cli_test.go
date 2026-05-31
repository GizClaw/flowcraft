package locomo

import (
	"testing"

	memorybbh "github.com/GizClaw/flowcraft/memory/retrieval/bbh"
	memoryretrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
	sdkbbh "github.com/GizClaw/flowcraft/sdk/retrieval/bbh"
	sdkretrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestBuildRetrievalIndexSelectsRunnerBackend(t *testing.T) {
	tests := []struct {
		name      string
		runner    string
		backend   string
		wantV1BBH bool
		wantV2BBH bool
	}{
		{name: "v1 memory", runner: runnerFlowcraftRecallV1, backend: "memory"},
		{name: "v1 default memory", runner: runnerFlowcraftRecallV1},
		{name: "v1 bbh", runner: runnerFlowcraftRecallV1, backend: "bbh", wantV1BBH: true},
		{name: "v2 memory", runner: runnerFlowcraftRecallV2, backend: "memory"},
		{name: "v2 default memory", runner: runnerFlowcraftRecallV2},
		{name: "v2 bbh", runner: runnerFlowcraftRecallV2, backend: "bbh", wantV2BBH: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v1, v2, cleanup, err := buildRetrievalIndex(tt.runner, tt.backend, t.TempDir())
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				t.Fatal(err)
			}
			if v1 != nil {
				defer v1.Close()
			}
			if v2 != nil {
				defer v2.Close()
			}
			switch tt.runner {
			case runnerFlowcraftRecallV1:
				if v1 == nil || v2 != nil {
					t.Fatalf("v1=%T v2=%T, want only v1 index", v1, v2)
				}
				if tt.wantV1BBH {
					if _, ok := v1.(*sdkbbh.Index); !ok {
						t.Fatalf("v1 index = %T, want sdk bbh", v1)
					}
				} else if _, ok := v1.(*sdkretrievalmem.Index); !ok {
					t.Fatalf("v1 index = %T, want sdk memory", v1)
				}
			case runnerFlowcraftRecallV2:
				if v1 != nil || v2 == nil {
					t.Fatalf("v1=%T v2=%T, want only v2 index", v1, v2)
				}
				if tt.wantV2BBH {
					if _, ok := v2.(*memorybbh.Index); !ok {
						t.Fatalf("v2 index = %T, want memory bbh", v2)
					}
				} else if _, ok := v2.(*memoryretrievalmem.Index); !ok {
					t.Fatalf("v2 index = %T, want memory memory", v2)
				}
			}
		})
	}
}

func TestBuildRetrievalIndexRejectsUnknownBackend(t *testing.T) {
	if _, _, _, err := buildRetrievalIndex(runnerFlowcraftRecallV1, "bad", t.TempDir()); err == nil {
		t.Fatal("unknown backend should fail")
	}
}
