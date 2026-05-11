// Command eval runs a single knowledge retrieval-quality evaluation
// across one or more lanes (bm25 / vector / hybrid) and writes a JSON
// Report. Pairs with eval/locomo/cmd/eval and eval/history/cmd/eval so
// CI and nightly dashboards see uniformly-shaped artifacts.
//
// Usage:
//
//	# BM25-only smoke (no credentials required)
//	go run ./eval/knowledge/cmd/eval \
//	    --corpus eval/knowledge/testdata/corpus \
//	    --golden eval/knowledge/testdata/golden.jsonl \
//	    --lanes  bm25
//
//	# full lane comparison against Qwen embeddings (FLOWCRAFT_QWEN must
//	# be set; lanes auto-skip with reason="embedder not configured"
//	# when --embedder is empty)
//	export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"text-embedding-v4"}'
//	go run ./eval/knowledge/cmd/eval \
//	    --corpus    eval/knowledge/testdata/corpus \
//	    --golden    eval/knowledge/testdata/golden.jsonl \
//	    --embedder  qwen:text-embedding-v4 \
//	    --lanes     bm25,vector,hybrid \
//	    --out       knowledge-report.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	knowledgequality "github.com/GizClaw/flowcraft/eval/knowledge"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/sdk/knowledge"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
)

func main() {
	corpus := flag.String("corpus", "eval/knowledge/testdata/corpus", "directory of *.md documents to index")
	golden := flag.String("golden", "eval/knowledge/testdata/golden.jsonl", "JSONL golden question set")
	embedderSpec := flag.String("embedder", "", "embedder spec (provider:model), e.g. qwen:text-embedding-v4; empty restricts to BM25 lane")
	lanesFlag := flag.String("lanes", "bm25,vector,hybrid", "comma-separated subset of {bm25,vector,hybrid}")
	topK := flag.Int("topk", knowledgequality.DefaultTopK, "rank cutoff for Recall@K")
	concurrency := flag.Int("concurrency", 4, "in-flight searches per lane")
	negCeiling := flag.Float64("negative-score-ceiling", 0, "negative-class queries whose top-1 score exceeds this count as breaches (0 disables)")
	out := flag.String("out", "", "output report path (default: stdout)")
	notifyFlags := notify.RegisterFlags(flag.CommandLine)
	flag.Parse()

	notifier, err := notifyFlags.Build()
	if err != nil {
		log.Fatalf("notify: %v", err)
	}

	emb, err := env.BuildEmbedder(*embedderSpec)
	if err != nil {
		log.Fatalf("--embedder: %v", err)
	}

	ds, err := knowledgequality.LoadDatasetFromDir(*corpus, *golden)
	if err != nil {
		log.Fatalf("load dataset: %v", err)
	}

	lanes, err := parseLanes(*lanesFlag)
	if err != nil {
		log.Fatalf("--lanes: %v", err)
	}

	opts := knowledgequality.Options{
		Embedder:             emb,
		Lanes:                lanes,
		TopK:                 *topK,
		Concurrency:          *concurrency,
		NegativeScoreCeiling: *negCeiling,
		ProgressPct:          *notifyFlags.ProgressPct,
		Hook: func(ctx context.Context, e knowledgequality.Event) {
			notify.Forward(ctx, notifier, notify.Event{
				Kind:   e.Kind,
				Time:   e.Time,
				Title:  e.Title,
				Body:   e.Body,
				Fields: e.Fields,
			})
		},
	}

	rep, err := knowledgequality.Run(context.Background(), ds, opts)
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

	// Operator-friendly summary on stderr regardless of --out, so a
	// pipeline that captures stdout for the JSON still sees the
	// human-readable verdict in its logs.
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
			r.Lane, *topK, r.RecallAtK, r.KeywordRate, r.NegativeBreach, r.Errors, r.LatencyP95)
	}
}

// parseLanes converts a `bm25,vector,hybrid` flag value into the
// strongly-typed lane slice consumed by knowledgequality.Run. We accept
// the canonical lane names exactly as sdk/knowledge.SearchMode emits
// them; mis-spelled lanes fail loud rather than silently dropping.
func parseLanes(s string) ([]knowledgequality.Lane, error) {
	parts := strings.Split(s, ",")
	out := make([]knowledgequality.Lane, 0, len(parts))
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
