package locomo

import (
	"testing"

	memorybbh "github.com/GizClaw/flowcraft/memory/retrieval/bbh"
	memoryretrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

func TestBuildRetrievalIndexSelectsBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		wantBBH bool
	}{
		{name: "memory", backend: "memory"},
		{name: "default memory"},
		{name: "bbh", backend: "bbh", wantBBH: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, cleanup, err := buildRetrievalIndex(tt.backend, t.TempDir())
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				t.Fatal(err)
			}
			defer idx.Close()
			if tt.wantBBH {
				if _, ok := idx.(*memorybbh.Index); !ok {
					t.Fatalf("index = %T, want memory bbh", idx)
				}
			} else if _, ok := idx.(*memoryretrievalmem.Index); !ok {
				t.Fatalf("index = %T, want memory memory", idx)
			}
		})
	}
}

func TestBuildRetrievalIndexRejectsUnknownBackend(t *testing.T) {
	if _, _, err := buildRetrievalIndex("bad", t.TempDir()); err == nil {
		t.Fatal("unknown backend should fail")
	}
}
