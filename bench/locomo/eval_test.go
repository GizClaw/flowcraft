package locomo_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/bench/locomo"
	"github.com/GizClaw/flowcraft/bench/locomo/dataset"
	"github.com/GizClaw/flowcraft/bench/locomo/runners/flowcraft"
)

func TestRunSyntheticDataset(t *testing.T) {
	ctx := context.Background()
	r, err := flowcraft.New(flowcraft.Options{Name: "test"})
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
