package simpleqa

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

// RegisterCobra attaches the `simpleqa` subcommand to parent. It is
// the suite's only public CLI surface; the package's `Run(ctx, ds,
// opts)` core remains untouched so tests + downstream callers stay
// stable across the cobra migration.
//
// Suite-specific flags live on this cmd; cross-cutting concerns
// (--notify-*, --env-file, --out, --verbose) come from g and are
// shared with every other suite.
func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	var (
		datasetPath        string
		answerLLMSpec      string
		judgeLLMSpec       string
		concurrency        int
		limit              int
		perQTimeout        time.Duration
		maxSamples         int
		includeTopic       bool
	)

	cmd := &cobra.Command{
		Use:   "simpleqa",
		Short: "SimpleQA short-form factuality + calibration (LLM-as-judge)",
		Long: `Run OpenAI's SimpleQA benchmark and emit a JSON Report.

Headline metric is "attempted accuracy" = CORRECT / (CORRECT + INCORRECT).
A model that abstains rather than hallucinates scores higher than one
that confidently answers wrong — exactly the behaviour we want from a
reliable agent backbone.

Example:
  curl -L https://openaipublic.blob.core.windows.net/simple-evals/simple_qa_test_set.csv \
    -o /tmp/simple_qa.csv
  eval simpleqa --dataset /tmp/simple_qa.csv \
      --answer-llm qwen:qwen-max --judge-llm azure --out /tmp/sqa.json`,
		RunE: func(c *cobra.Command, _ []string) error {
			if datasetPath == "" {
				return fmt.Errorf("--dataset is required")
			}
			if answerLLMSpec == "" {
				return fmt.Errorf("--answer-llm is required (e.g. qwen:qwen-max)")
			}
			if judgeLLMSpec == "" {
				return fmt.Errorf("--judge-llm is required (e.g. azure)")
			}
			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			answer, err := env.BuildLLM(answerLLMSpec)
			if err != nil {
				return fmt.Errorf("--answer-llm: %w", err)
			}
			if answer == nil {
				return fmt.Errorf("--answer-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
			}
			judge, err := env.BuildLLM(judgeLLMSpec)
			if err != nil {
				return fmt.Errorf("--judge-llm: %w", err)
			}
			if judge == nil {
				return fmt.Errorf("--judge-llm resolved to nil; check FLOWCRAFT_<ALIAS> env var")
			}

			ds, err := LoadDataset(datasetPath)
			if err != nil {
				return fmt.Errorf("load dataset: %w", err)
			}
			log.Printf("[simpleqa] loaded %d questions from %s", len(ds.Questions), ds.Name)

			opts := Options{
				AnswerLLM:             answer,
				JudgeLLM:              judge,
				Concurrency:           concurrency,
				LimitQuestions:        limit,
				MaxSamples:            maxSamples,
				PerQuestionTimeout:    perQTimeout,
				IncludeTopicBreakdown: includeTopic,
				ProgressPct:           g.Notify.ProgressPct,
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
			rep.Model = answerLLMSpec
			rep.Judge = judgeLLMSpec
			if err := g.WriteReport(rep); err != nil {
				return err
			}

			// Operator summary to stderr; --out and stdout-JSON
			// paths both reach this so a pipeline gets the verdict
			// without parsing the JSON.
			fmt.Fprintf(os.Stderr,
				"  n=%d correct=%d incorrect=%d not_attempted=%d judge_failures=%d\n"+
					"  accuracy=%.3f attempted_accuracy=%.3f abstention=%.3f hallucination=%.3f\n",
				rep.N, rep.Correct, rep.Incorrect, rep.NotAttempted, rep.JudgeFailures,
				rep.Accuracy, rep.AttemptedAccuracy, rep.AbstentionRate, rep.HallucinationRate,
			)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&datasetPath, "dataset", "", "path to simple_qa_test_set.csv (or .jsonl converted form); required")
	f.StringVar(&answerLLMSpec, "answer-llm", "", "model under test, format provider:model (required)")
	f.StringVar(&judgeLLMSpec, "judge-llm", "", "judge LLM, format provider:model (required)")
	f.IntVar(&concurrency, "concurrency", 4, "parallel (answer, judge) pairs")
	f.IntVar(&limit, "limit", 0, "evaluate only the first N questions (0 = all)")
	f.DurationVar(&perQTimeout, "per-question-timeout", 90*time.Second, "deadline for a single answer+judge pair; 0 disables")
	f.IntVar(&maxSamples, "max-samples", 200, "cap on Report.Samples (debug detail rows)")
	f.BoolVar(&includeTopic, "include-topic-breakdown", true, "populate Report.PerTopic")

	parent.AddCommand(cmd)
}
