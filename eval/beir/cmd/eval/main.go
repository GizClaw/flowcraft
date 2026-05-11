// Command eval runs a BEIR-format retrieval evaluation and writes a
// JSON Report. Pairs with eval/locomo/cmd/eval, eval/history/cmd/eval
// and eval/knowledge/cmd/eval — same notify.CLIFlags surface, same
// report shape ergonomics.
//
// Quick start (SciFact is the smallest BEIR task, ~1k docs / 300 queries):
//
//	# 1. Fetch SciFact once (BEIR redistributes everything as zip files).
//	curl -L https://public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/scifact.zip -o /tmp/scifact.zip
//	unzip -q /tmp/scifact.zip -d /tmp
//
//	# 2. BM25 lane (no credentials).
//	go run ./eval/beir/cmd/eval \
//	    --root  /tmp/scifact \
//	    --lanes bm25
//
//	# 3. Full lane comparison.
//	export FLOWCRAFT_QWEN='{"api_key":"...","model":"text-embedding-v4"}'
//	go run ./eval/beir/cmd/eval \
//	    --root      /tmp/scifact \
//	    --embedder  qwen:text-embedding-v4 \
//	    --lanes     bm25,vector,hybrid \
//	    --out       /tmp/beir-scifact.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/eval/beir"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/sdk/knowledge"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
)

func main() {
	root := flag.String("root", "", "path to a BEIR dataset root (must contain corpus.jsonl, queries.jsonl, qrels/test.tsv)")
	embedderSpec := flag.String("embedder", "", "embedder spec, e.g. qwen:text-embedding-v4; empty restricts to BM25")
	lanesFlag := flag.String("lanes", "bm25,vector,hybrid", "comma-separated subset of {bm25,vector,hybrid}")
	cutoffsFlag := flag.String("cutoffs", "10,100", "comma-separated nDCG / Recall rank cutoffs")
	ingestConcurrency := flag.Int("ingest-concurrency", 8, "concurrent PutDocument calls during ingest")
	queryConcurrency := flag.Int("query-concurrency", 8, "concurrent Search calls during scoring")
	limitQueries := flag.Int("limit-queries", 0, "evaluate only the first N queries (0 = all)")
	out := flag.String("out", "", "output report path (default: stdout)")
	notifyFlags := notify.RegisterFlags(flag.CommandLine)
	flag.Parse()

	if *root == "" {
		log.Fatal("--root is required")
	}
	notifier, err := notifyFlags.Build()
	if err != nil {
		log.Fatalf("notify: %v", err)
	}
	emb, err := env.BuildEmbedder(*embedderSpec)
	if err != nil {
		log.Fatalf("--embedder: %v", err)
	}
	ds, err := beir.LoadDataset(*root)
	if err != nil {
		log.Fatalf("load BEIR dataset: %v", err)
	}
	lanes, err := parseLanes(*lanesFlag)
	if err != nil {
		log.Fatalf("--lanes: %v", err)
	}
	cutoffs, err := parseInts(*cutoffsFlag)
	if err != nil {
		log.Fatalf("--cutoffs: %v", err)
	}

	opts := beir.Options{
		Embedder:          emb,
		Lanes:             lanes,
		Cutoffs:           cutoffs,
		IngestConcurrency: *ingestConcurrency,
		QueryConcurrency:  *queryConcurrency,
		LimitQueries:      *limitQueries,
		ProgressPct:       *notifyFlags.ProgressPct,
		Hook: func(ctx context.Context, e beir.Event) {
			notify.Forward(ctx, notifier, notify.Event{
				Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
			})
		},
	}

	rep, err := beir.Run(context.Background(), ds, opts)
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if *out == "" {
		fmt.Println(string(b))
	} else {
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			log.Fatalf("write: %v", err)
		}
		fmt.Printf("wrote %s\n", *out)
	}

	// Human-friendly summary on stderr regardless of --out so pipelines
	// that capture stdout still see the verdict in their logs.
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
}

// parseLanes is a duplicate of eval/knowledge/cmd/eval's helper. Kept
// separate so each binary's --lanes flag can grow independently
// (e.g. BEIR could add a "splade" lane in the future without
// disturbing knowledge's flag surface).
func parseLanes(s string) ([]beir.Lane, error) {
	parts := strings.Split(s, ",")
	out := make([]beir.Lane, 0, len(parts))
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

func parseInts(s string) ([]int, error) {
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
