package knowledgequality

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/sdk/knowledge"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
)

// RegisterCobra attaches the `knowledge` subcommand to parent.
func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	var (
		corpus       string
		golden       string
		embedderSpec string
		lanesFlag    string
		topK         int
		concurrency  int
		negCeiling   float64
	)

	cmd := &cobra.Command{
		Use:   "knowledge",
		Short: "Hand-curated knowledge retrieval regression (BM25 / vector / hybrid)",
		Long: `Run the FlowCraft knowledge retrieval-quality regression. Pairs with
"eval beir" for public-dataset comparisons; this suite is the
deterministic PR-gate fixture (100 docs / 40 questions).

Example:
  eval knowledge \
      --corpus eval/knowledge/testdata/corpus \
      --golden eval/knowledge/testdata/golden.jsonl \
      --lanes  bm25`,
		RunE: func(c *cobra.Command, _ []string) error {
			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			emb, err := env.BuildEmbedder(embedderSpec)
			if err != nil {
				return fmt.Errorf("--embedder: %w", err)
			}
			ds, err := LoadDatasetFromDir(corpus, golden)
			if err != nil {
				return fmt.Errorf("load dataset: %w", err)
			}
			lanes, err := parseLanesFlag(lanesFlag)
			if err != nil {
				return fmt.Errorf("--lanes: %w", err)
			}

			opts := Options{
				Embedder:             emb,
				Lanes:                lanes,
				TopK:                 topK,
				Concurrency:          concurrency,
				NegativeScoreCeiling: negCeiling,
				ProgressPct:          g.Notify.ProgressPct,
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
				},
			}

			rep, err := Run(c.Context(), ds, opts)
			if err != nil {
				return fmt.Errorf("run: %w", err)
			}
			if err := g.WriteReport(rep); err != nil {
				return err
			}
			for _, lane := range lanes {
				r := rep.Lanes[lane]
				if r == nil {
					continue
				}
				if r.Skipped != "" {
					fmt.Fprintf(os.Stderr, "  %-7s SKIPPED (%s)\n", r.Lane, r.Skipped)
					continue
				}
				fmt.Fprintf(os.Stderr, "  %-7s recall@%d=%.3f keyword=%.3f negBreach=%d errors=%d p95=%s\n",
					r.Lane, topK, r.RecallAtK, r.KeywordRate, r.NegativeBreach, r.Errors, r.LatencyP95)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&corpus, "corpus", "eval/knowledge/testdata/corpus", "directory of *.md documents to index")
	f.StringVar(&golden, "golden", "eval/knowledge/testdata/golden.jsonl", "JSONL golden question set")
	f.StringVar(&embedderSpec, "embedder", "", "embedder spec (provider:model), e.g. qwen:text-embedding-v4; empty restricts to BM25 lane")
	f.StringVar(&lanesFlag, "lanes", "bm25,vector,hybrid", "comma-separated subset of {bm25,vector,hybrid}")
	f.IntVar(&topK, "topk", DefaultTopK, "rank cutoff for Recall@K")
	f.IntVar(&concurrency, "concurrency", 4, "in-flight searches per lane")
	f.Float64Var(&negCeiling, "negative-score-ceiling", 0, "negative-class queries whose top-1 score exceeds this count as breaches (0 disables)")

	parent.AddCommand(cmd)
}

func parseLanesFlag(s string) ([]Lane, error) {
	parts := strings.Split(s, ",")
	out := make([]Lane, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch knowledge.SearchMode(p) {
		case knowledge.ModeBM25, knowledge.ModeVector, knowledge.ModeHybrid:
			out = append(out, knowledge.SearchMode(p))
		default:
			return nil, fmt.Errorf("unknown lane %q (want bm25 / vector / hybrid)", p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no lanes specified")
	}
	return out, nil
}
