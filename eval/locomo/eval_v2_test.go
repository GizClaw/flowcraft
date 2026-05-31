package locomo_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraftv2"
	retrievalmem "github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

func TestRunSyntheticDataset_flowcraftV2(t *testing.T) {
	ctx := context.Background()
	r, err := flowcraftv2.New(flowcraftv2.Options{
		Name:           "flowcraft-recall-v2",
		RetrievalIndex: retrievalmem.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	ds := dataset.Synthetic()
	report, err := locomo.Run(ctx, r, ds, locomo.Options{TopK: 5, UseExtractor: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.RecallVersion != "v2" {
		t.Fatalf("RecallVersion = %q, want v2", report.RecallVersion)
	}
	if report.Baseline != flowcraftv2.Baseline {
		t.Fatalf("Baseline = %q, want %q", report.Baseline, flowcraftv2.Baseline)
	}
	if report.Runner != "flowcraft-recall-v2" {
		t.Fatalf("Runner = %q", report.Runner)
	}
	if report.N != len(ds.Questions) {
		t.Fatalf("expected N=%d, got %d", len(ds.Questions), report.N)
	}
	if report.Latency["save"].N == 0 || report.Latency["recall"].N == 0 {
		t.Fatalf("missing latency: %+v", report.Latency)
	}
}
