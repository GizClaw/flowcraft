package locomo

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/eval/internal/notify"
	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/localmem"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/sdk/llm"

	_ "github.com/GizClaw/flowcraft/sdkx/embedding/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/embedding/qwen"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/azure"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen"
)

func RegisterCobra(parent *cobra.Command, g *cliflags.Global) {
	var (
		datasetPath         string
		workspaceRoot       string
		answerSpec          string
		memorySpec          string
		judgeSpec           string
		embedderSpec        string
		tasksSpec           string
		limitSamples        int
		limitQA             int
		excludeQACategories []int
		qaTopK              int
		limitTurns          int
		concurrency         int
		qaConcurrency       int
		maxSamples          int
		perCallTimeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "locomo",
		Short: "LoCoMo memory eval: QA, event summary, caption-proxy dialog",
		Long: `Run a memory-backed LoCoMo eval suite.

The suite uses LocalWorkspace + synchronous memory writes. Multimodal dialog
generation is reported as caption_proxy: image URL, BLIP caption, and query
metadata are used as text memory inputs; this does not claim official visual MM-R parity.`,
		RunE: func(c *cobra.Command, _ []string) error {
			if datasetPath == "" {
				return fmt.Errorf("--dataset is required")
			}
			if workspaceRoot == "" {
				return fmt.Errorf("--workspace is required")
			}
			if answerSpec == "" {
				return fmt.Errorf("--answer-llm is required")
			}
			tasks, err := ParseTasks(tasksSpec)
			if err != nil {
				return err
			}
			notifier, err := g.Notify.Build()
			if err != nil {
				return fmt.Errorf("notify: %w", err)
			}
			answerLLM, err := env.BuildLLM(answerSpec)
			if err != nil {
				return fmt.Errorf("--answer-llm: %w", err)
			}
			var memoryLLM llm.LLM
			if memorySpec != "" {
				memoryLLM, err = env.BuildLLM(memorySpec)
				if err != nil {
					return fmt.Errorf("--memory-llm: %w", err)
				}
			}
			var judgeLLM llm.LLM
			if judgeSpec != "" {
				judgeLLM, err = env.BuildLLM(judgeSpec)
				if err != nil {
					return fmt.Errorf("--judge-llm: %w", err)
				}
			}
			embedder, err := env.BuildEmbedder(embedderSpec)
			if err != nil {
				return fmt.Errorf("--embedder: %w", err)
			}
			ds, err := dataset.Load(datasetPath)
			if err != nil {
				return fmt.Errorf("load dataset: %w", err)
			}
			log.Printf("[locomo] loaded %d samples from %s", len(ds.Samples), ds.Name)
			mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
				WorkspaceRoot:  workspaceRoot,
				Embedder:       embedder,
				MemoryLLM:      memoryLLM,
				PerCallTimeout: perCallTimeout,
			})
			if err != nil {
				return fmt.Errorf("memory: %w", err)
			}
			defer func() {
				if err := closeMem(); err != nil {
					log.Printf("[locomo] close memory: %v", err)
				}
			}()
			rep, err := Run(c.Context(), ds, Options{
				Memory:              mem,
				AnswerLLM:           answerLLM,
				JudgeLLM:            judgeLLM,
				WorkspaceRoot:       workspaceRoot,
				RunID:               "locomo-" + fmt.Sprint(os.Getpid()),
				Tasks:               tasks,
				LimitSamples:        limitSamples,
				LimitQA:             limitQA,
				ExcludeQACategories: excludeQACategories,
				QATopK:              qaTopK,
				LimitTurns:          limitTurns,
				Concurrency:         concurrency,
				QAConcurrency:       qaConcurrency,
				MaxSamples:          maxSamples,
				ProgressPct:         g.Notify.ProgressPct,
				PerCallTimeout:      perCallTimeout,
				ReportHook:          progressReportHook(g.OutPath),
				Hook: func(ctx context.Context, e Event) {
					notify.Forward(ctx, notifier, notify.Event{
						Kind: e.Kind, Time: e.Time, Title: e.Title, Body: e.Body, Fields: e.Fields,
					})
				},
			})
			if err != nil {
				return fmt.Errorf("run: %w", err)
			}
			rep.Options["answer_llm"] = answerSpec
			rep.Options["memory_llm"] = memorySpec
			rep.Options["judge_llm"] = judgeSpec
			rep.Options["embedder"] = embedderSpec
			if err := g.WriteReport(rep); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  %s\n", locomoreport.SummaryLine(rep))
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&datasetPath, "dataset", "", "path to locomo10.json; required")
	f.StringVar(&workspaceRoot, "workspace", "", "unique LocalWorkspace root for this run; required")
	f.StringVar(&answerSpec, "answer-llm", "", "LLM used to answer QA/event/dialog prompts; required")
	f.StringVar(&memorySpec, "memory-llm", "", "optional LLM used to derive SummaryDAG recall-bridge nodes")
	f.StringVar(&judgeSpec, "judge-llm", "", "optional LLM judge for semantic QA correctness scoring")
	f.StringVar(&embedderSpec, "embedder", "", "optional embedder for hybrid source-message retrieval")
	f.StringVar(&tasksSpec, "tasks", "qa,event,dialog", "comma-separated tasks: qa,event,dialog")
	f.IntVar(&limitSamples, "limit-samples", 0, "evaluate only first N samples (0 = all)")
	f.IntVar(&limitQA, "limit-qa", 0, "evaluate only first N QA items per sample (0 = all)")
	f.IntSliceVar(&excludeQACategories, "exclude-qa-category", nil, "QA category IDs to exclude before applying --limit-qa (repeat or comma-separated, e.g. 5)")
	f.IntVar(&qaTopK, "qa-top-k", defaultQATopK, "maximum final source messages used for LoCoMo QA context")
	f.IntVar(&limitTurns, "limit-turns", 0, "ingest only first N turns per sample for smoke runs (0 = all)")
	f.IntVar(&concurrency, "concurrency", 4, "maximum concurrent LoCoMo samples/conversations")
	f.IntVar(&qaConcurrency, "qa-concurrency", 4, "maximum concurrent QA items within one LoCoMo sample")
	f.IntVar(&maxSamples, "max-samples", 200, "cap Report.Samples debug rows")
	f.DurationVar(&perCallTimeout, "per-call-timeout", 0, "timeout for individual answer LLM and supported memory calls (0 = no limit)")

	parent.AddCommand(cmd)
}

func progressReportHook(outPath string) ReportHook {
	if outPath == "" {
		return nil
	}
	return func(_ context.Context, rep *locomoreport.Report) error {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(outPath, data, 0o644)
	}
}
