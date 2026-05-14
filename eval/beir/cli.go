package beir

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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

// RegisterCobra attaches the `beir` subcommand to parent. See package
// doc for scoring detail; this function only owns the CLI wire-up.
func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	var (
		root              string
		embedderSpec      string
		lanesFlag         string
		cutoffsFlag       string
		ingestConcurrency int
		queryConcurrency  int
		limitQueries      int
		overfetch         int
	)

	cmd := &cobra.Command{
		Use:   "beir",
		Short: "BEIR-format public retrieval benchmark (nDCG@k / Recall@k / MRR)",
		Long: `Run a BEIR-format retrieval evaluation against sdk/knowledge.

The BEIR three-file layout (corpus.jsonl + queries.jsonl + qrels/test.tsv)
loads as-is — no converter required. Scoring uses graded nDCG@k,
binary Recall@k, and MRR — the metrics every BEIR leaderboard
publishes.

Example:
  curl -L https://public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/scifact.zip \
    -o /tmp/scifact.zip
  unzip -q /tmp/scifact.zip -d /tmp
  eval beir --root /tmp/scifact --lanes bm25 --out /tmp/scifact.json`,
		RunE: func(c *cobra.Command, _ []string) error {
			if root == "" {
				return fmt.Errorf("--root is required")
			}
			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			emb, err := env.BuildEmbedder(embedderSpec)
			if err != nil {
				return fmt.Errorf("--embedder: %w", err)
			}
			ds, err := LoadDataset(root)
			if err != nil {
				return fmt.Errorf("load BEIR dataset: %w", err)
			}
			lanes, err := parseLanesFlag(lanesFlag)
			if err != nil {
				return fmt.Errorf("--lanes: %w", err)
			}
			cutoffs, err := parseIntsFlag(cutoffsFlag)
			if err != nil {
				return fmt.Errorf("--cutoffs: %w", err)
			}

			opts := Options{
				Embedder:          emb,
				Lanes:             lanes,
				Cutoffs:           cutoffs,
				IngestConcurrency: ingestConcurrency,
				QueryConcurrency:  queryConcurrency,
				LimitQueries:      limitQueries,
				OverfetchFactor:   overfetch,
				ProgressPct:       g.Notify.ProgressPct,
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
					// Mirror milestone events to stderr so operators
					// running the binary directly (or tailing
					// `nohup`-style logs) get the same progress signal
					// the Feishu webhook receives — silent multi-minute
					// ingest is the #1 "is it stuck?" page driver. The
					// allow-list keeps lower-resolution debug emits off
					// the operator's screen.
					switch e.Kind {
					case "start", "ingest_start", "ingest_progress", "ingest_done",
						"lane_start", "lane_progress", "lane_done", "done", "error":
						body := e.Body
						if e.Title != "" && body == "" {
							body = e.Title
						}
						fmt.Fprintf(os.Stderr, "[%s] %s %s\n",
							time.Now().Format("15:04:05"), e.Kind, body)
					}
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
				var parts []string
				for _, k := range cutoffs {
					parts = append(parts, fmt.Sprintf("nDCG@%d=%.3f recall@%d=%.3f", k, r.NDCG[k], k, r.Recall[k]))
				}
				fmt.Fprintf(os.Stderr, "  %-7s %s mrr=%.3f errors=%d p95=%s\n",
					r.Lane, strings.Join(parts, " "), r.MRR, r.Errors, r.LatencyP95)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&root, "root", "", "path to a BEIR dataset root (must contain corpus.jsonl, queries.jsonl, qrels/test.tsv)")
	f.StringVar(&embedderSpec, "embedder", "", "embedder spec, e.g. qwen:text-embedding-v4; empty restricts to BM25")
	f.StringVar(&lanesFlag, "lanes", "bm25,vector,hybrid", "comma-separated subset of {bm25,vector,hybrid}")
	f.StringVar(&cutoffsFlag, "cutoffs", "10,100", "comma-separated nDCG / Recall rank cutoffs")
	f.IntVar(&ingestConcurrency, "ingest-concurrency", 8, "concurrent PutDocument calls during ingest")
	f.IntVar(&queryConcurrency, "query-concurrency", 8, "concurrent Search calls during scoring")
	f.IntVar(&limitQueries, "limit-queries", 0, "evaluate only the first N queries (0 = all)")
	f.IntVar(&overfetch, "overfetch", DefaultOverfetchFactor,
		"chunk over-fetch factor applied before chunks→docID collapse "+
			"(1 = disable collapse; ablation only — non-conformant with BEIR protocol)")

	parent.AddCommand(cmd)
}

// parseLanesFlag mirrors the inline helper from the old binary.
// Kept private to the package so each suite's --lanes can drift
// independently if BEIR ever grows a "splade" lane that knowledge
// doesn't have.
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

func parseIntsFlag(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q: %w", p, err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("cutoff must be > 0, got %d", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no cutoffs specified")
	}
	return out, nil
}
