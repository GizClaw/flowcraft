// Command eval runs the SimpleQA benchmark and writes a JSON Report.
//
// SimpleQA grades a model's short-form factuality (4 326 questions
// across diverse topics) using the official LLM-as-judge rubric. The
// headline metric is "attempted accuracy" = CORRECT / (CORRECT +
// INCORRECT), which rewards calibration: a model that abstains rather
// than hallucinates scores higher even at a lower raw accuracy.
//
// Quick start:
//
//	# 1. Fetch the upstream CSV once.
//	curl -L https://openaipublic.blob.core.windows.net/simple-evals/simple_qa_test_set.csv \
//	    -o /tmp/simple_qa_test_set.csv
//
//	# 2. Run with the model under test and a separate judge.
//	export FLOWCRAFT_QWEN='{"api_key":"sk-...","model":"qwen-max"}'
//	export FLOWCRAFT_AZURE='{"api_key":"...","model":"gpt-5","base_url":"..."}'
//	go run ./eval/simpleqa/cmd/eval \
//	    --dataset    /tmp/simple_qa_test_set.csv \
//	    --answer-llm qwen:qwen-max \
//	    --judge-llm  azure \
//	    --concurrency 8 \
//	    --out        /tmp/simpleqa-qwenmax.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/simpleqa"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

func main() {
	datasetPath := flag.String("dataset", "", "path to simple_qa_test_set.csv (or .jsonl converted form); required")
	answerLLM := flag.String("answer-llm", "", "model under test, format provider:model (required)")
	judgeLLM := flag.String("judge-llm", "", "judge LLM, format provider:model (required)")
	concurrency := flag.Int("concurrency", 4, "parallel (answer, judge) pairs")
	limit := flag.Int("limit", 0, "evaluate only the first N questions (0 = all)")
	perQTimeout := flag.Duration("per-question-timeout", 90*time.Second, "deadline for a single answer+judge pair; 0 disables")
	maxSamples := flag.Int("max-samples", 200, "cap on Report.Samples (debug detail rows)")
	includeTopic := flag.Bool("include-topic-breakdown", true, "populate Report.PerTopic")
	out := flag.String("out", "", "output report path (default: stdout)")
	notifyFlags := notify.RegisterFlags(flag.CommandLine)
	flag.Parse()

	if *datasetPath == "" {
		log.Fatal("--dataset is required")
	}
	if *answerLLM == "" {
		log.Fatal("--answer-llm is required (e.g. qwen:qwen-max)")
	}
	if *judgeLLM == "" {
		log.Fatal("--judge-llm is required (e.g. azure)")
	}
	notifier, err := notifyFlags.Build()
	if err != nil {
		log.Fatalf("notify: %v", err)
	}
	answer, err := env.BuildLLM(*answerLLM)
	if err != nil {
		log.Fatalf("--answer-llm: %v", err)
	}
	if answer == nil {
		log.Fatal("--answer-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
	}
	judge, err := env.BuildLLM(*judgeLLM)
	if err != nil {
		log.Fatalf("--judge-llm: %v", err)
	}
	if judge == nil {
		log.Fatal("--judge-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
	}

	ds, err := simpleqa.LoadDataset(*datasetPath)
	if err != nil {
		log.Fatalf("load dataset: %v", err)
	}
	log.Printf("[simpleqa] loaded %d questions from %s", len(ds.Questions), ds.Name)

	opts := simpleqa.Options{
		AnswerLLM:             answer,
		JudgeLLM:              judge,
		Concurrency:           *concurrency,
		LimitQuestions:        *limit,
		MaxSamples:            *maxSamples,
		PerQuestionTimeout:    *perQTimeout,
		IncludeTopicBreakdown: *includeTopic,
		ProgressPct:           *notifyFlags.ProgressPct,
		Hook: func(ctx context.Context, e simpleqa.Event) {
			notify.Forward(ctx, notifier, notify.Event{
				Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
			})
		},
	}

	rep, err := simpleqa.Run(context.Background(), ds, opts)
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	rep.Model = *answerLLM
	rep.Judge = *judgeLLM

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

	// Operator-friendly verdict on stderr regardless of --out.
	fmt.Fprintf(os.Stderr,
		"  n=%d correct=%d incorrect=%d not_attempted=%d judge_failures=%d\n"+
			"  accuracy=%.3f attempted_accuracy=%.3f abstention=%.3f hallucination=%.3f\n",
		rep.N, rep.Correct, rep.Incorrect, rep.NotAttempted, rep.JudgeFailures,
		rep.Accuracy, rep.AttemptedAccuracy, rep.AbstentionRate, rep.HallucinationRate,
	)
}
