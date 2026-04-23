package main

import (
	"path/filepath"
	"testing"
)

func TestLoadSpecAndLint(t *testing.T) {
	root := filepath.Join("..", "..")
	sp, err := loadSpec(filepath.Join(root, "contracts/events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Events) != 32 {
		t.Fatalf("expected 32 events, got %d", len(sp.Events))
	}
	if errs := lintSpec(sp); len(errs) > 0 {
		for _, e := range errs {
			t.Error(e)
		}
	}
}

func TestCheckEvolutionBaseline(t *testing.T) {
	root := filepath.Join("..", "..")
	sp, err := loadSpec(filepath.Join(root, "contracts/events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bp := filepath.Join(root, "cmd/eventgen/testdata/baseline/events.snapshot.yaml")
	if evo := checkEvolution(sp, bp); len(evo) > 0 {
		for _, e := range evo {
			t.Error(e)
		}
	}
}
