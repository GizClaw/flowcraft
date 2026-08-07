package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/memory/eval/scenarios"
)

const (
	defaultMaxItems    = 20
	defaultMaxTokens   = 4096
	defaultConcurrency = 4
)

func usage() {
	fmt.Fprint(os.Stderr, `memory-eval: LoCoMo / LongMemEval benchmark for the flowcraft memory module.

Usage:
  memory-eval run [flags]
  memory-eval convert [flags]
  memory-eval help

Run flags:
  --dataset PATH             converted .jsonl dataset
  --inference PATH           inference.yaml (providers + env secret refs)
  --suite NAME               locomo (default) or longmemeval
  --generate-model SPEC      provider:model[:profile] for fact extraction
  --embed-model SPEC         provider:model[:profile] for the vector lane
  --answer-model SPEC        provider:model[:profile] for QA answers
  --judge-model SPEC         provider:model[:profile] for LLM judging (empty disables)
  --max-items N              ContextRequest budget (default 20)
  --max-tokens N             ContextRequest budget (default 4096)
  --limit N                  cap questions to the first N (0 = all)
  --limit-conversations N    cap conversations to the first N (0 = all)
  --concurrency N            QA worker count (default 4)
  --ingest-timeout D         per-conversation ingest+derive deadline (default 0)
  --qa-timeout D             per-question recall+answer+judge deadline (default 0)
  --commit-granularity G     session, exchange, or turn (default session)
  --out PATH                 report.json destination
  --notify-name NAME         run identifier shown in Feishu card header
  --notify-progress-pct N    milestone notifications every N percent (0 disables)
  --notify-dry-run           print events to stderr instead of posting to Feishu

Convert flags:
  --suite NAME               locomo or longmemeval
  --in PATH                  upstream JSON
  --out PATH                 converted .jsonl
  --limit N                  cap upstream instances to the first N (0 = all)
`)
}

func runCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		datasetPath       string
		inferencePath     string
		suite             string
		generateModel     string
		embedModel        string
		answerModel       string
		judgeModel        string
		out               string
		maxItems          int
		maxTokens         int
		limit             int
		limitConvs        int
		concurrency       int
		ingestTimeout     time.Duration
		qaTimeout         time.Duration
		commitGranularity string
		verbose           bool
		notifyName        string
		notifyPct         int
		notifyDryRun      bool
	)
	fs.StringVar(&datasetPath, "dataset", "", "converted dataset (.jsonl)")
	fs.StringVar(&inferencePath, "inference", "", "inference.yaml")
	fs.StringVar(&suite, "suite", "locomo", "locomo or longmemeval")
	fs.StringVar(&generateModel, "generate-model", "", "provider:model[:profile]")
	fs.StringVar(&embedModel, "embed-model", "", "provider:model[:profile]")
	fs.StringVar(&answerModel, "answer-model", "", "provider:model[:profile]")
	fs.StringVar(&judgeModel, "judge-model", "", "provider:model[:profile]")
	fs.StringVar(&out, "out", "", "report.json destination")
	fs.IntVar(&maxItems, "max-items", defaultMaxItems, "ContextRequest max items")
	fs.IntVar(&maxTokens, "max-tokens", defaultMaxTokens, "ContextRequest max tokens")
	fs.IntVar(&limit, "limit", 0, "cap questions to the first N")
	fs.IntVar(&limitConvs, "limit-conversations", 0, "cap conversations to the first N")
	fs.IntVar(&concurrency, "concurrency", defaultConcurrency, "QA worker count")
	fs.DurationVar(&ingestTimeout, "ingest-timeout", 0, "per-conversation ingest deadline")
	fs.DurationVar(&qaTimeout, "qa-timeout", 0, "per-question QA deadline")
	fs.StringVar(&commitGranularity, "commit-granularity", string(granularitySession), "session, exchange, or turn")
	fs.BoolVar(&verbose, "verbose", false, "print the full wrapped error chain on failure")
	fs.StringVar(&notifyName, "notify-name", "", "run identifier shown in Feishu card header")
	fs.IntVar(&notifyPct, "notify-progress-pct", 25, "milestone notifications every N percent")
	fs.BoolVar(&notifyDryRun, "notify-dry-run", false, "print events to stderr instead of posting to Feishu")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if datasetPath == "" || inferencePath == "" || generateModel == "" ||
		embedModel == "" || answerModel == "" || out == "" {
		return fmt.Errorf("run requires --dataset, --inference, --generate-model, --embed-model, --answer-model, and --out")
	}
	scenario, err := scenarios.Lookup(suite)
	if err != nil {
		return err
	}
	granularity, err := parseCommitGranularity(commitGranularity)
	if err != nil {
		return err
	}
	ds, err := dataset.Load(datasetPath)
	if err != nil {
		return err
	}
	dataset.ApplyConversationLimit(ds, limitConvs)
	report, err := Run(ctx, runOptions{
		Dataset:           ds,
		InferencePath:     inferencePath,
		Scenario:          scenario,
		GenerateModel:     generateModel,
		EmbedModel:        embedModel,
		AnswerModel:       answerModel,
		JudgeModel:        judgeModel,
		MaxItems:          maxItems,
		MaxTokens:         maxTokens,
		Limit:             limit,
		Concurrency:       concurrency,
		IngestTimeout:     ingestTimeout,
		QATimeout:         qaTimeout,
		CommitGranularity: granularity,
		Notifier:          buildNotifier(notifyName, notifyDryRun),
		ProgressPct:       notifyPct,
	})
	if err != nil {
		if verbose {
			return fmt.Errorf("%w\nfull chain:\n%s", err, errorChain(err))
		}
		return err
	}
	return writeReport(out, report)
}

func convertCommand(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	var (
		suite string
		in    string
		out   string
		limit int
	)
	fs.StringVar(&suite, "suite", "", "locomo or longmemeval")
	fs.StringVar(&in, "in", "", "upstream JSON")
	fs.StringVar(&out, "out", "", "converted .jsonl")
	fs.IntVar(&limit, "limit", 0, "cap upstream instances to the first N")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if in == "" || out == "" {
		return fmt.Errorf("convert requires --suite, --in, and --out")
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	scenario, err := scenarios.Lookup(suite)
	if err != nil {
		return err
	}
	ds, stats, convErr := scenario.Convert(raw)
	if convErr != nil {
		return fmt.Errorf("convert %s: %w", scenario.Name(), convErr)
	}
	fmt.Fprintf(os.Stderr, "%s conversion: %s\n", scenario.Name(), stats.String())
	dataset.ApplyConversationLimit(&ds, limit)
	file, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := dataset.Write(file, ds); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	fmt.Printf("wrote %s: %d conversations, %d questions\n", out, len(ds.Conversations), len(ds.Questions))
	return nil
}

func errorChain(err error) string {
	var lines []string
	seen := make(map[error]struct{})
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if _, ok := seen[current]; ok {
			return
		}
		seen[current] = struct{}{}
		lines = append(lines, current.Error())
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range multi.Unwrap() {
				walk(child)
			}
			return
		}
		walk(errors.Unwrap(current))
	}
	walk(err)
	return strings.Join(lines, "\n")
}
