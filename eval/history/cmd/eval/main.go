// Command eval runs a single history-compression evaluation and writes a JSON
// report. See eval/history/doc.go for the methodology and CLI examples.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/history"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/metrics"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

func main() {
	datasetFlag := flag.String("dataset", "synthetic", "dataset (synthetic) or path to .jsonl")
	answerLLM := flag.String("answer-llm", "", "LLM that answers using the loaded history, format provider:model (required)")
	summaryLLM := flag.String("summary-llm", "", "LLM the compactor uses to summarize older turns; if empty, the 'compacted' strategy is skipped")
	judgeLLM := flag.String("judge-llm", "", "LLM-as-judge model, format provider:model; empty falls back to EMJudge")
	bufferMax := flag.Int("buffer-max", 10, "StrategyBuffer window")
	tokenBudget := flag.Int("compact-token-budget", 1500, "StrategyCompacted token budget per Load (also used as the per-Load Budget for none/buffer)")
	limitConvs := flag.Int("limit-convs", 0, "evaluate only the first N conversations (debug)")
	limitQs := flag.Int("limit-questions", 0, "evaluate only the first N questions (debug)")
	stratFlag := flag.String("strategies", "none,buffer,compacted", "comma-separated subset of {none, buffer, compacted}")
	concurrency := flag.Int("concurrency", 1, "questions answered in parallel per strategy (default 1 = sequential)")
	progressEvery := flag.Int("progress-every", 0, "log a progress line every N completed questions per strategy (0 = silent)")
	out := flag.String("out", "", "output report path (default: stdout)")
	notifyFlags := notify.RegisterFlags(flag.CommandLine)
	flag.Parse()

	notifier, err := notifyFlags.Build()
	if err != nil {
		log.Fatalf("notify: %v", err)
	}

	ans, err := env.BuildLLM(*answerLLM)
	if err != nil {
		log.Fatalf("--answer-llm: %v", err)
	}
	if ans == nil {
		log.Fatal("--answer-llm is required (e.g. qwen:qwen-max)")
	}
	sum, err := env.BuildLLM(*summaryLLM)
	if err != nil {
		log.Fatalf("--summary-llm: %v", err)
	}
	judge, err := env.BuildLLM(*judgeLLM)
	if err != nil {
		log.Fatalf("--judge-llm: %v", err)
	}

	var ds *dataset.Dataset
	if *datasetFlag == "synthetic" {
		ds = dataset.Synthetic()
	} else {
		ds, err = dataset.LoadJSONL(*datasetFlag)
		if err != nil {
			log.Fatalf("load dataset: %v", err)
		}
	}

	opts := history.Options{
		AnswerLLM:          ans,
		SummaryLLM:         sum,
		BufferMax:          *bufferMax,
		CompactTokenBudget: *tokenBudget,
		LimitConvs:         *limitConvs,
		LimitQs:            *limitQs,
		Strategies:         parseStrategies(*stratFlag),
		Concurrency:        *concurrency,
		ProgressEvery:      *progressEvery,
		ProgressPct:        *notifyFlags.ProgressPct,
		Hook: func(ctx context.Context, e history.Event) {
			notify.Forward(ctx, notifier, notify.Event{
				Kind:   e.Kind,
				Time:   e.Time,
				Title:  e.Title,
				Body:   e.Body,
				Fields: e.Fields,
			})
		},
	}
	if judge != nil {
		opts.Judge = metrics.LLMJudge{LLM: judge}
	}

	rep, err := history.Run(context.Background(), ds, opts)
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	b, err := json.MarshalIndent(rep, "", "  ")
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
	fmt.Printf("wrote %s\n", *out)
	for _, s := range rep.Strategies {
		if s.Skipped != "" {
			fmt.Printf("  %-9s SKIPPED (%s)\n", s.Strategy, s.Skipped)
			continue
		}
		trunc := "n/a"
		if s.EvidenceMeasured > 0 {
			trunc = fmt.Sprintf("%d/%d (%.1f%%)", s.Truncated, s.EvidenceMeasured, s.TruncatedRate*100)
		}
		fmt.Printf("  %-9s judge=%.3f f1=%.3f em=%.3f tokens.p95=%d load.p95=%s truncated=%s\n",
			s.Strategy, s.Judge, s.F1, s.EM, s.PromptTokensP95, s.LoadLatencyP95, trunc)
	}
}

func parseStrategies(s string) []history.Strategy {
	parts := strings.Split(s, ",")
	out := make([]history.Strategy, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, history.Strategy(p))
	}
	return out
}
