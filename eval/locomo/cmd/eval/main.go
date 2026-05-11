// Command eval runs a single evaluation and writes a JSON report.
//
// Usage:
//
//	# cheap CI run (no API keys, retrieval-only): EM/F1/k_hit but qa.judge ≈ EM
//	go run ./eval/locomo/cmd/eval --dataset eval/locomo/data/locomo10.jsonl --out r.json
//
//	# full LLM-driven run: LLM extractor + LLM answer + LLM judge + Qwen embedder
//	export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
//	go run ./eval/locomo/cmd/eval \
//	    --dataset      eval/locomo/data/locomo10.jsonl \
//	    --extractor                                  \
//	    --answer-llm   qwen:qwen-max                 \
//	    --judge-llm    qwen:qwen-max                 \
//	    --embedder     qwen:text-embedding-v4        \
//	    --out          r.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/locomo"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraft"
	"github.com/GizClaw/flowcraft/eval/metrics"

	// Side-effect imports register the providers we accept on the CLI.
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

func main() {
	runner := flag.String("runner", "flowcraft", "runner name (currently: flowcraft)")
	datasetFlag := flag.String("dataset", "synthetic", "dataset (synthetic) or path to .jsonl")
	out := flag.String("out", "", "output report path (default: stdout)")
	topK := flag.Int("topk", 10, "Recall top-k")
	useExtractor := flag.Bool("extractor", false, "use LLM extractor on Save (requires --extractor-llm or shared --answer-llm)")
	extractorLLM := flag.String("extractor-llm", "", "LLM for fact extraction, format provider:model (e.g. qwen:qwen-max); falls back to --answer-llm")
	answerLLM := flag.String("answer-llm", "", "LLM that synthesizes the answer from top-k memories, format provider:model")
	judgeLLM := flag.String("judge-llm", "", "LLM-as-Judge model, format provider:model; if empty uses EMJudge")
	embedderFlag := flag.String("embedder", "", "embedder, format provider:model (e.g. qwen:text-embedding-v4); enables vector lane")
	limitConvs := flag.Int("limit-convs", 0, "if >0, evaluate only the first N conversations (debug)")
	limitQs := flag.Int("limit-questions", 0, "if >0, evaluate only the first N questions (debug)")
	concurrency := flag.Int("concurrency", 1, "QA-loop parallelism")
	ingestConcurrency := flag.Int("ingest-concurrency", 1, "per-conversation extractor batch parallelism (1=sequential); raise to 8-16 to parallelize session-sliced Save calls")
	progressEvery := flag.Int("progress-every", 0, "log every N completed questions; 0 disables")
	ingestTimeout := flag.Duration("ingest-timeout", 10*time.Minute, "per-conversation ingest deadline; bounds hung LLM calls")
	qaTimeout := flag.Duration("qa-timeout", 2*time.Minute, "per-question recall+answer+judge deadline")
	maxFacts := flag.Int("max-facts", 200, "extractor: max facts per Save call")
	tunedPrompts := flag.Bool("tuned-prompts", true, "use the LoCoMo-tuned extractor & answer prompts (recommended)")
	rerankerLLM := flag.String("reranker-llm", "", "LLM for cross-encoder rerank, format provider:model; empty disables")
	judgeStyle := flag.String("judge-style", "locomo", "judge prompt style: locomo (mem0-aligned, lenient) | strict (semantic-equivalence)")
	judgeTemp := flag.Float64("judge-temperature", 0.0, "judge LLM temperature (0=deterministic, mem0-aligned)")
	scoreThreshold := flag.Float64("score-threshold", 0, "drop recall hits below this score before rerank/limit (0 = SDK default 0.05)")
	saveWithContext := flag.Bool("save-with-context", false, "before extraction, recall existing facts and inject as prompt context")
	softMerge := flag.Bool("soft-merge", true, "mark older near-duplicate entries as superseded_by; SupersededDecay damps them at recall")
	notifyName := flag.String("notify-name", "", "run identifier shown in the Feishu card header (e.g. lme-oracle); empty disables prefix")
	notifyProgressPct := flag.Int("notify-progress-pct", 25, "send milestone notifications every N percent of ingest + QA (0 disables intermediate updates)")
	notifyDryRun := flag.Bool("notify-dry-run", false, "print events to stderr instead of posting to Feishu (for CI / smoke tests)")
	flag.Parse()

	if *runner != "flowcraft" {
		log.Fatalf("unknown runner: %s", *runner)
	}

	// Load + optionally truncate dataset.
	var ds *dataset.Dataset
	var err error
	if *datasetFlag == "synthetic" {
		ds = dataset.Synthetic()
	} else {
		ds, err = dataset.LoadJSONL(*datasetFlag)
		if err != nil {
			log.Fatalf("load dataset: %v", err)
		}
	}
	if *limitConvs > 0 && len(ds.Conversations) > *limitConvs {
		ds.Conversations = ds.Conversations[:*limitConvs]
		// also drop questions whose conversation_id is no longer present
		keep := map[string]struct{}{}
		for _, c := range ds.Conversations {
			keep[c.ID] = struct{}{}
		}
		filtered := ds.Questions[:0]
		for _, q := range ds.Questions {
			if _, ok := keep[q.ConversationID]; ok {
				filtered = append(filtered, q)
			}
		}
		ds.Questions = filtered
	}
	if *limitQs > 0 && len(ds.Questions) > *limitQs {
		ds.Questions = ds.Questions[:*limitQs]
	}

	// Build provider-backed components.
	extractor, err := env.BuildLLM(*extractorLLM)
	if err != nil {
		log.Fatalf("--extractor-llm: %v", err)
	}
	answer, err := env.BuildLLM(*answerLLM)
	if err != nil {
		log.Fatalf("--answer-llm: %v", err)
	}
	judge, err := env.BuildLLM(*judgeLLM)
	if err != nil {
		log.Fatalf("--judge-llm: %v", err)
	}
	embedder, err := env.BuildEmbedder(*embedderFlag)
	if err != nil {
		log.Fatalf("--embedder: %v", err)
	}
	reranker, err := env.BuildLLM(*rerankerLLM)
	if err != nil {
		log.Fatalf("--reranker-llm: %v", err)
	}
	if *useExtractor && extractor == nil {
		extractor = answer
	}

	rOpts := flowcraft.Options{
		Name:             *runner,
		LLM:              extractor,
		Embedder:         embedder,
		MaxFactsPerCall:  *maxFacts,
		IncludeAssistant: true,
		SaveWithContext:  *saveWithContext,
		SoftMerge:        softMerge,
		RerankerLLM:      reranker,
		ScoreThreshold:   *scoreThreshold,
	}
	if *tunedPrompts {
		rOpts.ExtractPrompt = locomoExtractorPrompt
	}
	r, err := flowcraft.New(rOpts)
	if err != nil {
		log.Fatalf("runner: %v", err)
	}
	defer r.Close()

	// Feishu CardKit credentials are read from env so they never appear in
	// `ps` output and can be shared across all eval CLIs. If any of the
	// three is missing, FromFlags falls back to NoOp.
	notifier, err := notify.FromFlags(notify.FlagOptions{
		Name:            *notifyName,
		DryRun:          *notifyDryRun,
		FeishuAppID:     os.Getenv("FEISHU_APP_ID"),
		FeishuAppSecret: os.Getenv("FEISHU_APP_SECRET"),
		FeishuChatID:    os.Getenv("FEISHU_CHAT_ID"),
	})
	if err != nil {
		log.Fatalf("notify: %v", err)
	}
	opts := locomo.Options{
		TopK:              *topK,
		UseExtractor:      *useExtractor,
		AnswerLLM:         answer,
		Concurrency:       *concurrency,
		IngestConcurrency: *ingestConcurrency,
		ProgressEvery:     *progressEvery,
		IngestTimeout:     *ingestTimeout,
		QATimeout:         *qaTimeout,
		ProgressPct:       *notifyProgressPct,
		Hook: func(ctx context.Context, e locomo.Event) {
			if err := notifier.Notify(ctx, notify.Event{
				Kind: e.Kind, Time: e.Time,
				Title: e.Title, Body: e.Body, Fields: e.Fields,
			}); err != nil {
				log.Printf("[notify] %s: %v", e.Kind, err)
			}
		},
	}
	if *tunedPrompts {
		opts.AnswerPrompt = locomoAnswerPrompt
	}
	if judge != nil {
		j := metrics.LLMJudge{LLM: judge, Temperature: judgeTemp}
		switch *judgeStyle {
		case "locomo", "mem0":
			j.Prompt = metrics.LocoMoLLMJudgePrompt
		case "strict", "default":
			// keep empty → DefaultLLMJudgePrompt
		default:
			log.Fatalf("--judge-style: unknown %q (want locomo|strict)", *judgeStyle)
		}
		opts.Judge = j
	}

	report, err := locomo.Run(context.Background(), r, ds, opts)
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if *out == "" {
		fmt.Println(string(b))
		return
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatalf("write: %v", err)
	}
	khit := "N/A"
	if report.Aggregate.KHitRate != nil {
		khit = fmt.Sprintf("%.3f", *report.Aggregate.KHitRate)
	}
	fmt.Printf("wrote %s (qa.judge=%.3f qa.f1=%.3f recall.k_hit=%s save.p95=%s recall.p95=%s)\n",
		*out, report.Aggregate.Judge, report.Aggregate.F1, khit,
		report.Latency["save"].P95, report.Latency["recall"].P95)
}

// locomoExtractorPrompt is tuned for LoCoMo-style multi-session conversations:
//   - turns are pre-formatted as "[<datetime>] <speaker>: <text>" by the
//     converter, so the model can attribute each fact to a date and a person.
//   - we explicitly ask for many small facts (LoCoMo conversations span 35+
//     sessions; capping at ~20 facts/conv silently drops 90% of the corpus).
//   - timestamps must be embedded inline so retrieval-time text matching can
//     surface them for cat2 (temporal) questions.
const locomoExtractorPrompt = `You are a long-term memory extractor for multi-session dialogues. Read the conversation below and extract every distinct, self-contained fact.

GUIDELINES
1. Emit ONE fact per atomic claim — preferences, profile attributes, events, plans, decisions, opinions, relationships. Do not bundle multiple claims.
2. ALWAYS embed the date the fact was said in the content itself (e.g. "On 7 May 2023, Caroline went to an LGBTQ support group"). Use the timestamp prefix [<datetime>] that appears at the start of each turn.
3. Attribute facts to the SPEAKER by name, not "the user" or "the assistant" — both speakers contribute facts in this dialog format.
4. Do NOT deduplicate. Different time points of the same kind of fact are separate facts.
5. Skip pure greetings / acknowledgements / single-emoji turns.
6. Be exhaustive: a 30-session conversation should produce 100+ facts.

OUTPUT FORMAT — strict JSON object with a single "facts" array, no prose, no fences:
{
  "facts": [
    {
      "content": "On 8 May 2023, Caroline mentioned she joined an LGBTQ support group.",
      "categories": ["episodic", "events"],
      "entities": ["Caroline", "LGBTQ support group", "8 May 2023"],
      "source": "user",
      "confidence": 0.95
    }
  ]
}

If no facts: return {"facts": []}.

%sCONVERSATION:
%s
`

// locomoAnswerPrompt is the QA prompt fed to AnswerLLM. Compared to the
// previous version this is intentionally NEUTRAL — earlier we had three
// "EM-friendly" rules (force minimal answers, force date-format mirroring,
// suppress IDK) that shifted bench numbers without reflecting real memory
// quality. The current version keeps only the two rules that are actually
// required for grounded QA:
//
//   - answer from the MEMORIES, not from prior knowledge
//   - paraphrase rather than verbatim-copy
//
// Length / format / IDK behavior are left to the model. Expected effects vs
// the prior tuned prompt (qa.judge=0.595, qa.f1=0.450, qa.em=0.252, IDK 1.7%):
// EM and F1 will drop a few points, judge mostly stable, IDK will rise from
// 1.7% to a more honest 5-10%.
const locomoAnswerPrompt = `Answer the QUESTION using only the MEMORIES below.

Guidelines:
- Ground the answer in the memories; do not invent facts that are not supported.
- Paraphrase in your own words rather than quoting verbatim.
- If the memories don't contain enough information, say so.

%s

Answer:`
