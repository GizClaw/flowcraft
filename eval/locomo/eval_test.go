package locomo_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraft"
	"github.com/GizClaw/flowcraft/sdk/retrieval/bbh"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestRunSyntheticDataset(t *testing.T) {
	ctx := context.Background()
	r, err := flowcraft.New(flowcraft.Options{Name: "test", RetrievalIndex: retrievalmem.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ds := dataset.Synthetic()

	report, err := locomo.Run(ctx, r, ds, locomo.Options{TopK: 5, UseExtractor: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.N != len(ds.Questions) {
		t.Fatalf("expected N=%d, got %d", len(ds.Questions), report.N)
	}
	if report.Aggregate.EM == 0 && report.Aggregate.F1 == 0 {
		t.Fatalf("expected at least one question to score; got %+v", report.Aggregate)
	}
	if report.Latency["save"].N == 0 || report.Latency["recall"].N == 0 {
		t.Fatalf("missing latency: %+v", report.Latency)
	}
}

func TestRunSyntheticDatasetV1BBH(t *testing.T) {
	ctx := context.Background()
	ws, err := sdkworkspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idx, err := bbh.New(ws)
	if err != nil {
		t.Fatal(err)
	}
	r, err := flowcraft.New(flowcraft.Options{Name: "flowcraft-recall-v1-bbh", RetrievalIndex: idx})
	if err != nil {
		_ = idx.Close()
		t.Fatal(err)
	}
	defer r.Close()

	ds := dataset.Synthetic()
	report, err := locomo.Run(ctx, r, ds, locomo.Options{TopK: 5, UseExtractor: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.RecallVersion != "v1" {
		t.Fatalf("RecallVersion = %q, want v1", report.RecallVersion)
	}
	if report.Runner != "flowcraft-recall-v1-bbh" {
		t.Fatalf("Runner = %q", report.Runner)
	}
	if report.N != len(ds.Questions) {
		t.Fatalf("expected N=%d, got %d", len(ds.Questions), report.N)
	}
	if report.Latency["save"].N == 0 || report.Latency["recall"].N == 0 {
		t.Fatalf("missing latency: %+v", report.Latency)
	}
}
