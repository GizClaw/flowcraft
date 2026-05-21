package locomo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraftv2"
	"github.com/GizClaw/flowcraft/eval/metrics"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/diagnostics"
	recallv1 "github.com/GizClaw/flowcraft/sdk/recall_v1"

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
	addLocomoAnalyzeRecall(group)
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
		rerankerLLM       string
		judgeStyle        string
		judgeTemp         float64
		scoreThreshold    float64
		saveWithContext   bool
		softMerge         bool
		multiRecall       bool
		entityStore       bool
		entityStoreMaxLnk int
		entityLinkBoost   float64
		queryEntityLLM    bool
		updateResolver    string
		recentTurnsK      int
		dumpFactsPath     string
		dumpRecallPath    string
		diagnosticsPath   string
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
			canonical, err := normalizeRunnerName(runnerName)
			if err != nil {
				return err
			}
			if canonical == "flowcraft-v2" && dumpFactsPath != "" {
				return fmt.Errorf("--dump-facts is not supported for flowcraft-v2 bootstrap")
			}

			var ds *dataset.Dataset
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
			var resolverLLM llm.LLM
			if updateResolver != "" {
				resolverLLM, err = env.BuildLLM(updateResolver)
				if err != nil {
					return fmt.Errorf("--update-resolver: %w", err)
				}
			}
			// Query-side entity extractor reuses the SAME LLM as the
			// write-side extractor — the whole point of opting in is
			// to make the two ends share a single entity vocabulary
			// so EntityStore.Lookup keys (saved at write time) actually
			// match the QueryEntities (extracted at recall time).
			// Defaulting to a different alias here would silently
			// re-introduce the asymmetry the feature exists to fix.
			var queryEntLLM llm.LLM
			if queryEntityLLM {
				queryEntLLM = extractor
			}
			if useExtractor && extractor == nil {
				extractor = answer
			}
			if canonical == "flowcraft-v2" && useExtractor && extractor == nil {
				return flowcraftv2.ErrExtractorNotSupported
			}

			// --dump-facts diagnostic: stream every Save batch's
			// extracted facts to a JSONL sidecar so we can audit
			// "extract miss vs recall miss" failures without
			// rerunning the index.
			var (
				dumpMu  sync.Mutex
				dumpW   *os.File
				dumpEnc *json.Encoder
				onFacts func(recallv1.Scope, []recallv1.ExtractedFact)
			)
			if dumpFactsPath != "" {
				dumpW, err = os.Create(dumpFactsPath)
				if err != nil {
					return fmt.Errorf("--dump-facts: %w", err)
				}
				defer dumpW.Close()
				dumpEnc = json.NewEncoder(dumpW)
				onFacts = func(scope recallv1.Scope, facts []recallv1.ExtractedFact) {
					dumpMu.Lock()
					defer dumpMu.Unlock()
					_ = dumpEnc.Encode(struct {
						TS    time.Time                `json:"ts"`
						Scope recallv1.Scope           `json:"scope"`
						Facts []recallv1.ExtractedFact `json:"facts"`
					}{time.Now(), scope, facts})
				}
			}

			// --diagnostics (v2 only): accumulate per-stage health across
			// the run so the operator can answer "where in the pipeline
			// is accuracy lost" from a single JSON.
			var (
				diagHealth *diagnostics.PipelineHealth
				diagMu     sync.Mutex
				v2Diag     *v2DiagnosticHooks
			)
			if diagnosticsPath != "" {
				if canonical != "flowcraft-v2" {
					return fmt.Errorf("--diagnostics is only supported for flowcraft-v2 (got %s)", canonical)
				}
				diagHealth = diagnostics.NewPipelineHealth()
				v2Diag = &v2DiagnosticHooks{
					OnSave: func(_ runners.Scope, d diagnostics.SaveDiagnostics) {
						diagMu.Lock()
						defer diagMu.Unlock()
						diagHealth.RecordSave(d)
					},
					OnRecall: func(_ runners.Scope, d diagnostics.RecallDiagnostics) {
						diagMu.Lock()
						defer diagMu.Unlock()
						diagHealth.RecordRecall(d)
					},
				}
			}

			r, err := buildLocomoRunner(canonical, v1RunnerConfig{
				LLM:                       extractor,
				Embedder:                  embedder,
				MaxFactsPerCall:           maxFacts,
				IncludeAssistant:          true,
				SaveWithContext:           saveWithContext,
				SoftMerge:                 &softMerge,
				RerankerLLM:               reranker,
				ScoreThreshold:            scoreThreshold,
				MultiRecall:               multiRecall,
				EntityStore:               entityStore,
				EntityStoreMaxLinkedCount: entityStoreMaxLnk,
				EntityLinkBoost:           entityLinkBoost,
				QueryEntityLLM:            queryEntLLM,
				UpdateResolverLLM:         resolverLLM,
				RecentTurnsK:              recentTurnsK,
				OnFactsExtracted:          onFacts,
			}, nil, v2Diag)
			if err != nil {
				return fmt.Errorf("runner: %w", err)
			}
			defer r.Close()

			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			// --dump-recall diagnostic: capture per-question recall
			// hits (id, score, content) to JSONL so we can audit
			// "recall miss vs answer miss" — does the retrieval
			// pipeline surface the gold-evidence fact at all?
			var (
				recallMu  sync.Mutex
				recallEnc *json.Encoder
				onRecall  func(q dataset.Question, hits []runners.Hit)
			)
			if dumpRecallPath != "" {
				rw, rerr := os.Create(dumpRecallPath)
				if rerr != nil {
					return fmt.Errorf("--dump-recall: %w", rerr)
				}
				defer rw.Close()
				recallEnc = json.NewEncoder(rw)
				onRecall = func(q dataset.Question, hits []runners.Hit) {
					recallMu.Lock()
					defer recallMu.Unlock()
					type hitRec struct {
						ID       string             `json:"id"`
						Rank     int                `json:"rank"`
						Score    float64            `json:"score"`
						Kind     string             `json:"kind,omitempty"`
						Sources  []string           `json:"sources,omitempty"`
						Content  string             `json:"content"`
						Evidence []string           `json:"evidence_ids,omitempty"`
						ValidAt  string             `json:"valid_from,omitempty"`
						Episodic bool               `json:"episodic,omitempty"`
						Cats     []string           `json:"categories,omitempty"`
						Scores   map[string]float64 `json:"scores,omitempty"`
					}
					recs := make([]hitRec, 0, len(hits))
					for i, h := range hits {
						rec := hitRec{
							ID:       h.ID,
							Rank:     i + 1,
							Score:    h.Score,
							Kind:     h.Kind,
							Sources:  append([]string(nil), h.Sources...),
							Content:  h.Content,
							Evidence: append([]string(nil), h.EvidenceIDs...),
							ValidAt:  h.ValidFrom,
						}
						if h.Metadata != nil {
							if cats, ok := h.Metadata["categories"].([]string); ok {
								rec.Cats = cats
							}
							if scores, ok := h.Metadata["scores"].(map[string]float64); ok {
								rec.Scores = scores
							}
						}
						recs = append(recs, rec)
					}
					_ = recallEnc.Encode(struct {
						TS    time.Time `json:"ts"`
						QID   string    `json:"qid"`
						Query string    `json:"query"`
						Gold  []string  `json:"gold_answers,omitempty"`
						Hits  []hitRec  `json:"hits"`
					}{time.Now(), q.ID, q.Query, q.GoldAnswers, recs})
				}
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
				OnQuestionRecall:  onRecall,
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
				},
			}
			// Answer prompt is intentionally not overridden: the
			// architecture-friendly rules (partial-info inference,
			// question-form mirror, date-format mirror, conciseness)
			// live in [DefaultAnswerPrompt] in this package so any
			// LoCoMo-shaped consumer gets them. Re-introducing a
			// LoCoMo-only overlay would risk silent drift between
			// eval scores and production behaviour.
			if judge != nil {
				j := metrics.LLMJudge{LLM: judge, Temperature: &judgeTemp}
				switch judgeStyle {
				case "locomo":
					j.Prompt = metrics.LocoMoLLMJudgePrompt
				case "default":
					// keep empty → DefaultLLMJudgePrompt
				default:
					return fmt.Errorf("--judge-style: unknown %q (want default|locomo)", judgeStyle)
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
			if diagHealth != nil {
				if err := writeDiagnosticsReport(diagnosticsPath, diagHealth); err != nil {
					return fmt.Errorf("write diagnostics: %w", err)
				}
				fmt.Fprintf(os.Stderr, "  diagnostics -> %s\n", diagnosticsPath)
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
			if bc := report.Aggregate.ByCategory; len(bc) > 0 {
				// Canonical ordering matches the mem0 / Memobase
				// publication tables so cross-project diff is a
				// line-by-line eyeball, not a column reorder game.
				for _, name := range []string{"single-hop", "temporal", "multi-hop", "open-domain", "adversarial"} {
					c, ok := bc[name]
					if !ok {
						continue
					}
					fmt.Fprintf(os.Stderr,
						"    %-12s n=%-4d qa.judge=%.3f qa.f1=%.3f qa.em=%.3f\n",
						name, c.Count, c.Judge, c.F1, c.EM,
					)
				}
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&runnerName, "runner", "flowcraft-v1", "runner: flowcraft-v1 (default) | flowcraft-v2 (v2 bootstrap)")
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
	f.StringVar(&rerankerLLM, "reranker-llm", "", "LLM for cross-encoder rerank, format provider:model; empty disables")
	f.StringVar(&judgeStyle, "judge-style", "default", "judge prompt style: default (FlowCraft answer-inclusion semantics) | locomo (leaderboard reproducer, lenient)")
	f.Float64Var(&judgeTemp, "judge-temperature", 0.0, "judge LLM temperature (0=deterministic)")
	f.Float64Var(&scoreThreshold, "score-threshold", 0, "drop recall hits below this score before rerank/limit (0 = SDK default 0.05)")
	f.BoolVar(&saveWithContext, "save-with-context", false, "before extraction, recall existing facts and inject as prompt context")
	f.BoolVar(&softMerge, "soft-merge", true, "mark older near-duplicate entries as superseded_by; SupersededDecay damps them at recall")
	f.BoolVar(&multiRecall, "multi-recall", false, "switch LTM to 3-lane recall (vector+bm25+entity) + RRFFusion; defaults to legacy single-lane vector recall + BM25/entity boosts")
	f.BoolVar(&entityStore, "entity-store", false, "enable the entity-link inverted index (4th MultiRetrieve lane); writes a sibling namespace per Save Link and adds a ModeEntityLink lane that materialises linked entries via DocGetter — auto-enables --multi-recall")
	f.IntVar(&entityStoreMaxLnk, "entity-store-max-linked", 0, "common-noun pollution gate: skip entity rows whose linked_ids count exceeds this threshold at Lookup time (0 = SDK safe default 100; negative = explicit opt-out (audited, see WithEntityStoreMaxLinkedCount godoc); positive = exact threshold)")
	f.Float64Var(&entityLinkBoost, "entity-link-boost", 0, "switch the entity-store integration from RRF lane to post-fusion score boost when > 0 (recommended 0.2-0.5); vector + BM25 own candidate generation, entity-link only re-ranks the fused result. Mitigates the lane-flooding regression that hits multi-hop questions when one entity dominates the namespace.")
	f.BoolVar(&queryEntityLLM, "query-entity-extractor", false, "swap the rule-based query-side entity extractor for an LLM call using the SAME LLM as --extractor-llm; closes the asymmetry between QueryEntities (capitalized single tokens) and the multi-word EntityStore keys (LLM-extracted noun phrases). Adds 1 LLM call per recall. No-op when --entity-store is false.")
	f.StringVar(&updateResolver, "update-resolver", "", "LLM alias for the memory update resolver (ADD/UPDATE/DELETE/NOOP); empty disables. Adds one LLM call per Save batch.")
	f.IntVar(&recentTurnsK, "recent-turns", 0, "if >0, inject the previous K messages from prior Save batches into the extractor for cross-batch pronoun/entity reference resolution")
	f.StringVar(&dumpFactsPath, "dump-facts", "", "diagnostic: write one JSONL record per Save batch with the extractor's facts to this path (audits extract-miss vs recall-miss)")
	f.StringVar(&dumpRecallPath, "dump-recall", "", "diagnostic: write one JSONL record per question with the top-k recall hits to this path (audits recall-miss vs answer-miss)")
	f.StringVar(&diagnosticsPath, "diagnostics", "", "diagnostic (flowcraft-v2 only): write per-stage Save+Recall health summary to this JSON path (uses SaveExplain/RecallExplain)")

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
