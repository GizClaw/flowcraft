// Command eval runs a single history-compression evaluation and writes a JSON
// report. See bench/history-compression/doc.go for the methodology and CLI
// examples.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	hc "github.com/GizClaw/flowcraft/bench/history-compression"
	"github.com/GizClaw/flowcraft/bench/locomo/dataset"
	"github.com/GizClaw/flowcraft/bench/locomo/metrics"
	"github.com/GizClaw/flowcraft/sdk/llm"

	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
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
	flag.Parse()

	ans, err := buildLLM(*answerLLM)
	if err != nil {
		log.Fatalf("--answer-llm: %v", err)
	}
	if ans == nil {
		log.Fatal("--answer-llm is required (e.g. qwen:qwen-max)")
	}
	sum, err := buildLLM(*summaryLLM)
	if err != nil {
		log.Fatalf("--summary-llm: %v", err)
	}
	judge, err := buildLLM(*judgeLLM)
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

	opts := hc.Options{
		AnswerLLM:          ans,
		SummaryLLM:         sum,
		BufferMax:          *bufferMax,
		CompactTokenBudget: *tokenBudget,
		LimitConvs:         *limitConvs,
		LimitQs:            *limitQs,
		Strategies:         parseStrategies(*stratFlag),
		Concurrency:        *concurrency,
		ProgressEvery:      *progressEvery,
	}
	if judge != nil {
		opts.Judge = metrics.LLMJudge{LLM: judge}
	}

	rep, err := hc.Run(context.Background(), ds, opts)
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

func parseStrategies(s string) []hc.Strategy {
	parts := strings.Split(s, ",")
	out := make([]hc.Strategy, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, hc.Strategy(p))
	}
	return out
}

// buildLLM mirrors bench/locomo/cmd/eval's helper. Kept verbatim so the two
// benches can drift independently without one breaking the other; the surface
// area is small enough that abstracting is not worth the cross-package
// dependency.
func buildLLM(spec string) (llm.LLM, error) {
	if spec == "" {
		return nil, nil
	}
	provider, model, ok := strings.Cut(spec, ":")
	if !ok || provider == "" {
		return nil, fmt.Errorf("expected provider:model, got %q", spec)
	}
	envPrefix := strings.ToUpper(provider)
	apiKey := os.Getenv(envPrefix + "_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("%s_API_KEY env var is empty", envPrefix)
	}
	cfg := map[string]any{"api_key": apiKey}
	if v := os.Getenv(envPrefix + "_BASE_URL"); v != "" {
		cfg["base_url"] = v
	}
	if v := os.Getenv(envPrefix + "_API_VERSION"); v != "" {
		cfg["api_version"] = v
	}
	caps := map[string]any{}
	if os.Getenv(envPrefix+"_NO_TEMPERATURE") != "" {
		caps["no_temperature"] = true
	}
	if os.Getenv(envPrefix+"_NO_JSON_SCHEMA") != "" {
		caps["no_json_schema"] = true
	}
	if os.Getenv(envPrefix+"_NO_JSON_MODE") != "" {
		caps["no_json_mode"] = true
	}
	if len(caps) > 0 {
		cfg["caps"] = caps
	}
	return llm.NewFromConfig(provider, model, cfg)
}
