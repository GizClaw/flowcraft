package history

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/metrics"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

// RegisterCobra attaches the `history` subcommand to parent.
func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	var (
		datasetFlag   string
		answerLLM     string
		summaryLLM    string
		judgeLLM      string
		bufferMax     int
		tokenBudget   int
		limitConvs    int
		limitQs       int
		stratFlag     string
		concurrency   int
		progressEvery int
	)

	cmd := &cobra.Command{
		Use:   "history",
		Short: "history-compression regression (none / buffer / compacted strategies)",
		Long: `Compare sdk/history compactor strategies on a LoCoMo-style dataset.

Reports judge / EM / F1 alongside prompt-token p95 and load latency so
operators can trade off cost vs. recall when picking a default
compactor for their stack.

Example:
  eval history --dataset synthetic \
      --answer-llm qwen:qwen-max --judge-llm azure`,
		RunE: func(c *cobra.Command, _ []string) error {
			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			ans, err := env.BuildLLM(answerLLM)
			if err != nil {
				return fmt.Errorf("--answer-llm: %w", err)
			}
			if ans == nil {
				return fmt.Errorf("--answer-llm is required (e.g. qwen:qwen-max)")
			}
			sum, err := env.BuildLLM(summaryLLM)
			if err != nil {
				return fmt.Errorf("--summary-llm: %w", err)
			}
			judge, err := env.BuildLLM(judgeLLM)
			if err != nil {
				return fmt.Errorf("--judge-llm: %w", err)
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

			opts := Options{
				AnswerLLM:          ans,
				SummaryLLM:         sum,
				BufferMax:          bufferMax,
				CompactTokenBudget: tokenBudget,
				LimitConvs:         limitConvs,
				LimitQs:            limitQs,
				Strategies:         parseStrategiesFlag(stratFlag),
				Concurrency:        concurrency,
				ProgressEvery:      progressEvery,
				ProgressPct:        g.Notify.ProgressPct,
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
				},
			}
			if judge != nil {
				opts.Judge = metrics.LLMJudge{LLM: judge}
			}

			rep, err := Run(c.Context(), ds, opts)
			if err != nil {
				return fmt.Errorf("run: %w", err)
			}
			if err := g.WriteReport(rep); err != nil {
				return err
			}
			for _, s := range rep.Strategies {
				if s.Skipped != "" {
					fmt.Fprintf(os.Stderr, "  %-9s SKIPPED (%s)\n", s.Strategy, s.Skipped)
					continue
				}
				trunc := "n/a"
				if s.EvidenceMeasured > 0 {
					trunc = fmt.Sprintf("%d/%d (%.1f%%)", s.Truncated, s.EvidenceMeasured, s.TruncatedRate*100)
				}
				fmt.Fprintf(os.Stderr, "  %-9s judge=%.3f f1=%.3f em=%.3f tokens.p95=%d load.p95=%s truncated=%s\n",
					s.Strategy, s.Judge, s.F1, s.EM, s.PromptTokensP95, s.LoadLatencyP95, trunc)
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&datasetFlag, "dataset", "synthetic", "dataset (synthetic) or path to .jsonl")
	f.StringVar(&answerLLM, "answer-llm", "", "LLM that answers using the loaded history, format provider:model (required)")
	f.StringVar(&summaryLLM, "summary-llm", "", "LLM the compactor uses to summarize older turns; if empty, the 'compacted' strategy is skipped")
	f.StringVar(&judgeLLM, "judge-llm", "", "LLM-as-judge model, format provider:model; empty falls back to EMJudge")
	f.IntVar(&bufferMax, "buffer-max", 10, "StrategyBuffer window")
	f.IntVar(&tokenBudget, "compact-token-budget", 1500, "StrategyCompacted token budget per Load (also used as the per-Load Budget for none/buffer)")
	f.IntVar(&limitConvs, "limit-convs", 0, "evaluate only the first N conversations (debug)")
	f.IntVar(&limitQs, "limit-questions", 0, "evaluate only the first N questions (debug)")
	f.StringVar(&stratFlag, "strategies", "none,buffer,compacted", "comma-separated subset of {none, buffer, compacted}")
	f.IntVar(&concurrency, "concurrency", 1, "questions answered in parallel per strategy (default 1 = sequential)")
	f.IntVar(&progressEvery, "progress-every", 0, "log a progress line every N completed questions per strategy (0 = silent)")

	parent.AddCommand(cmd)
}

func parseStrategiesFlag(s string) []Strategy {
	parts := strings.Split(s, ",")
	out := make([]Strategy, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, Strategy(p))
	}
	return out
}
