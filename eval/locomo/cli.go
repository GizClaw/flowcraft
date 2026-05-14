package locomo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraft"
	"github.com/GizClaw/flowcraft/eval/metrics"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

// RegisterCobra attaches the `locomo` command group to parent. Unlike
// the leaf suites (simpleqa/beir/taubench/…), LoCoMo ships with
// auxiliary tools — conversion, comparison, ingest — so the suite is
// a parent group with sub-subcommands:
//
//	eval locomo run        run an eval
//	eval locomo convert    upstream locomo10.json → .jsonl
//	eval locomo compare    diff two report JSONs
//	eval locomo fetch      print dataset fetch instructions
//	eval locomo ingest     ingest-only smoke (no QA)
func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	group := &cobra.Command{
		Use:   "locomo",
		Short: "LoCoMo-10 dialog-memory benchmark (run, convert, compare, fetch, ingest)",
		Long: `LoCoMo is the family of long-term dialog memory benchmarks shipped
in eval/locomo. Subcommands:

  run        execute the eval and emit a Report JSON
  convert    transform upstream snap-research locomo10.json → eval JSONL
  compare    markdown diff between two report JSONs
  fetch      print dataset fetch / clone instructions
  ingest     ingest-only loop (warm an index, observe extractor)`,
	}

	addLocomoRun(group, g)
	addLocomoConvert(group)
	addLocomoCompare(group)
	addLocomoFetch(group)
	addLocomoIngest(group)

	parent.AddCommand(group)
}

func addLocomoRun(parent *cobra.Command, g *cliflags.Global) {
	var (
		runnerName        string
		datasetFlag       string
		topK              int
		useExtractor      bool
		extractorLLM      string
		answerLLM         string
		judgeLLM          string
		embedderFlag      string
		limitConvs        int
		limitQs           int
		concurrency       int
		ingestConcurrency int
		progressEvery     int
		ingestTimeout     time.Duration
		qaTimeout         time.Duration
		maxFacts          int
		tunedPrompts      bool
		rerankerLLM       string
		judgeStyle        string
		judgeTemp         float64
		scoreThreshold    float64
		saveWithContext   bool
		softMerge         bool
		multiRecall       bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a LoCoMo-style evaluation",
		Long: `Run the LoCoMo memory-eval pipeline and write a JSON Report.

Example (LLM extractor + LLM answer + LLM judge + Qwen embedder):
  export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
  eval locomo run \
      --dataset eval/locomo/data/locomo10.jsonl \
      --extractor \
      --answer-llm qwen:qwen-max \
      --judge-llm  qwen:qwen-max \
      --embedder   qwen:text-embedding-v4 \
      --out r.json`,
		RunE: func(c *cobra.Command, _ []string) error {
			if runnerName != "flowcraft" {
				return fmt.Errorf("unknown runner: %s", runnerName)
			}

			var ds *dataset.Dataset
			var err error
			if datasetFlag == "synthetic" {
				ds = dataset.Synthetic()
			} else {
				ds, err = dataset.LoadJSONL(datasetFlag)
				if err != nil {
					return fmt.Errorf("load dataset: %w", err)
				}
			}
			if limitConvs > 0 && len(ds.Conversations) > limitConvs {
				ds.Conversations = ds.Conversations[:limitConvs]
				keep := map[string]struct{}{}
				for _, conv := range ds.Conversations {
					keep[conv.ID] = struct{}{}
				}
				filtered := ds.Questions[:0]
				for _, q := range ds.Questions {
					if _, ok := keep[q.ConversationID]; ok {
						filtered = append(filtered, q)
					}
				}
				ds.Questions = filtered
			}
			if limitQs > 0 && len(ds.Questions) > limitQs {
				ds.Questions = ds.Questions[:limitQs]
			}

			extractor, err := env.BuildLLM(extractorLLM)
			if err != nil {
				return fmt.Errorf("--extractor-llm: %w", err)
			}
			answer, err := env.BuildLLM(answerLLM)
			if err != nil {
				return fmt.Errorf("--answer-llm: %w", err)
			}
			judge, err := env.BuildLLM(judgeLLM)
			if err != nil {
				return fmt.Errorf("--judge-llm: %w", err)
			}
			embedder, err := env.BuildEmbedder(embedderFlag)
			if err != nil {
				return fmt.Errorf("--embedder: %w", err)
			}
			reranker, err := env.BuildLLM(rerankerLLM)
			if err != nil {
				return fmt.Errorf("--reranker-llm: %w", err)
			}
			if useExtractor && extractor == nil {
				extractor = answer
			}

			rOpts := flowcraft.Options{
				Name:             runnerName,
				LLM:              extractor,
				Embedder:         embedder,
				MaxFactsPerCall:  maxFacts,
				IncludeAssistant: true,
				SaveWithContext:  saveWithContext,
				SoftMerge:        &softMerge,
				RerankerLLM:      reranker,
				ScoreThreshold:   scoreThreshold,
				MultiRecall:      multiRecall,
			}
			if tunedPrompts {
				rOpts.ExtractPrompt = LocoMoExtractorPrompt
			}
			r, err := flowcraft.New(rOpts)
			if err != nil {
				return fmt.Errorf("runner: %w", err)
			}
			defer r.Close()

			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			opts := Options{
				TopK:              topK,
				UseExtractor:      useExtractor,
				AnswerLLM:         answer,
				Concurrency:       concurrency,
				IngestConcurrency: ingestConcurrency,
				ProgressEvery:     progressEvery,
				IngestTimeout:     ingestTimeout,
				QATimeout:         qaTimeout,
				ProgressPct:       g.Notify.ProgressPct,
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
				},
			}
			if tunedPrompts {
				opts.AnswerPrompt = LocoMoAnswerPrompt
			}
			if judge != nil {
				j := metrics.LLMJudge{LLM: judge, Temperature: &judgeTemp}
				switch judgeStyle {
				case "locomo", "mem0":
					j.Prompt = metrics.LocoMoLLMJudgePrompt
				case "strict", "default":
					// keep empty → DefaultLLMJudgePrompt
				default:
					return fmt.Errorf("--judge-style: unknown %q (want locomo|strict)", judgeStyle)
				}
				opts.Judge = j
			}

			report, err := Run(c.Context(), r, ds, opts)
			if err != nil {
				return fmt.Errorf("run: %w", err)
			}
			if err := g.WriteReport(report); err != nil {
				return err
			}
			khit := "N/A"
			if report.Aggregate.KHitRate != nil {
				khit = fmt.Sprintf("%.3f", *report.Aggregate.KHitRate)
			}
			fmt.Fprintf(os.Stderr,
				"  qa.judge=%.3f qa.f1=%.3f qa.em=%.3f recall.k_hit=%s save.p95=%s recall.p95=%s\n",
				report.Aggregate.Judge, report.Aggregate.F1, report.Aggregate.EM, khit,
				report.Latency["save"].P95, report.Latency["recall"].P95,
			)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&runnerName, "runner", "flowcraft", "runner name (currently: flowcraft)")
	f.StringVar(&datasetFlag, "dataset", "synthetic", "dataset (synthetic) or path to .jsonl")
	f.IntVar(&topK, "topk", 10, "Recall top-k")
	f.BoolVar(&useExtractor, "extractor", false, "use LLM extractor on Save (requires --extractor-llm or shared --answer-llm)")
	f.StringVar(&extractorLLM, "extractor-llm", "", "LLM for fact extraction, format provider:model; falls back to --answer-llm")
	f.StringVar(&answerLLM, "answer-llm", "", "LLM that synthesizes the answer from top-k memories, format provider:model")
	f.StringVar(&judgeLLM, "judge-llm", "", "LLM-as-Judge model, format provider:model; if empty uses EMJudge")
	f.StringVar(&embedderFlag, "embedder", "", "embedder, format provider:model (e.g. qwen:text-embedding-v4); enables vector lane")
	f.IntVar(&limitConvs, "limit-convs", 0, "if >0, evaluate only the first N conversations (debug)")
	f.IntVar(&limitQs, "limit-questions", 0, "if >0, evaluate only the first N questions (debug)")
	f.IntVar(&concurrency, "concurrency", 1, "QA-loop parallelism")
	f.IntVar(&ingestConcurrency, "ingest-concurrency", 1, "per-conversation extractor batch parallelism (1=sequential)")
	f.IntVar(&progressEvery, "progress-every", 0, "log every N completed questions; 0 disables")
	f.DurationVar(&ingestTimeout, "ingest-timeout", 10*time.Minute, "per-conversation ingest deadline; bounds hung LLM calls")
	f.DurationVar(&qaTimeout, "qa-timeout", 2*time.Minute, "per-question recall+answer+judge deadline")
	f.IntVar(&maxFacts, "max-facts", 200, "extractor: max facts per Save call")
	f.BoolVar(&tunedPrompts, "tuned-prompts", true, "use the LoCoMo-tuned extractor & answer prompts (recommended)")
	f.StringVar(&rerankerLLM, "reranker-llm", "", "LLM for cross-encoder rerank, format provider:model; empty disables")
	f.StringVar(&judgeStyle, "judge-style", "locomo", "judge prompt style: locomo (mem0-aligned, lenient) | strict (semantic-equivalence)")
	f.Float64Var(&judgeTemp, "judge-temperature", 0.0, "judge LLM temperature (0=deterministic, mem0-aligned)")
	f.Float64Var(&scoreThreshold, "score-threshold", 0, "drop recall hits below this score before rerank/limit (0 = SDK default 0.05)")
	f.BoolVar(&saveWithContext, "save-with-context", false, "before extraction, recall existing facts and inject as prompt context")
	f.BoolVar(&softMerge, "soft-merge", true, "mark older near-duplicate entries as superseded_by; SupersededDecay damps them at recall")
	f.BoolVar(&multiRecall, "multi-recall", false, "switch LTM to 3-lane recall (vector+bm25+entity) + RRFFusion; defaults to legacy single-lane vector recall + BM25/entity boosts")

	parent.AddCommand(cmd)
}

func addLocomoFetch(parent *cobra.Command) {
	var doDownload bool
	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Print dataset fetch / clone instructions",
		RunE: func(c *cobra.Command, _ []string) error {
			if !doDownload {
				fmt.Print(fetchInstructions)
				return nil
			}
			fmt.Fprintln(os.Stderr, "fetch: --download is not yet wired; clone manually:")
			fmt.Print(fetchInstructions)
			return nil
		},
	}
	cmd.Flags().BoolVar(&doDownload, "download", false, "actually download datasets (off by default; not yet wired)")
	parent.AddCommand(cmd)
}

const fetchInstructions = `# LoCoMo benchmark datasets
# Place under eval/locomo/data/<name>/ and reference via --dataset path.

# 1. LoCoMo (Snap Research, CC-BY)
git clone https://github.com/snap-research/locomo eval/locomo/data/locomo
# Then convert to .jsonl:
eval locomo convert \
    --in  eval/locomo/data/locomo/data/locomo10.json \
    --out eval/locomo/data/locomo10.jsonl

# 2. LongMemEval
git clone https://github.com/xiaowu0162/LongMemEval eval/locomo/data/longmemeval

# 3. Flowcraft synthetic — already bundled via dataset.Synthetic().
`

// load is shared by `compare` to deserialise a Report JSON.
func loadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Report{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, err
	}
	return r, nil
}

func addLocomoCompare(parent *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "compare <baseline.json> <current.json>",
		Short: "Markdown diff between two locomo report JSONs",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			base, err := loadReport(args[0])
			if err != nil {
				return err
			}
			cur, err := loadReport(args[1])
			if err != nil {
				return err
			}
			fmt.Printf("# locomo compare — %s vs %s\n\n", base.Runner, cur.Runner)
			fmt.Printf("- baseline: %s (%d questions)\n", base.Dataset, base.N)
			fmt.Printf("- current : %s (%d questions)\n\n", cur.Dataset, cur.N)
			fmt.Println("| metric | baseline | current | delta |")
			fmt.Println("|---|---:|---:|---:|")
			row("qa.em", base.Aggregate.EM, cur.Aggregate.EM, "%.3f")
			row("qa.f1", base.Aggregate.F1, cur.Aggregate.F1, "%.3f")
			row("qa.judge", base.Aggregate.Judge, cur.Aggregate.Judge, "%.3f")
			rowOpt("recall.k_hit", base.Aggregate.KHitRate, cur.Aggregate.KHitRate, "%.3f")
			rowDur("latency.save.p95", base.Latency["save"], cur.Latency["save"])
			rowDur("latency.recall.p95", base.Latency["recall"], cur.Latency["recall"])
			return nil
		},
	}
	parent.AddCommand(cmd)
}

func row(name string, a, b float64, fmtStr string) {
	delta := b - a
	fmt.Printf("| %s | "+fmtStr+" | "+fmtStr+" | %+.3f |\n", name, a, b, delta)
}

func rowOpt(name string, a, b *float64, fmtStr string) {
	if a == nil || b == nil {
		av, bv := "N/A", "N/A"
		if a != nil {
			av = fmt.Sprintf(fmtStr, *a)
		}
		if b != nil {
			bv = fmt.Sprintf(fmtStr, *b)
		}
		fmt.Printf("| %s | %s | %s | — |\n", name, av, bv)
		return
	}
	row(name, *a, *b, fmtStr)
}

func rowDur(name string, a, b metrics.LatencySummary) {
	delta := b.P95 - a.P95
	signed := delta.String()
	if delta >= 0 {
		signed = "+" + delta.String()
	}
	fmt.Printf("| %s | %s | %s | %s |\n", name, a.P95, b.P95, signed)
}
