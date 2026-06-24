package locomo

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/eval/locomo/tasks"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

const defaultQATopK = tasks.DefaultQATopK
const defaultQAGraphExpandedMaxSource = tasks.DefaultQAGraphExpandedMaxSource

type Event struct {
	Kind   string
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

type EventHook func(ctx context.Context, e Event)

type ReportHook func(ctx context.Context, rep *locomoreport.Report) error

type Options struct {
	Memory                   *memory.System
	AnswerLLM                llm.LLM
	JudgeLLM                 llm.LLM
	WorkspaceRoot            string
	RunID                    string
	Tasks                    []locomoreport.Task
	LimitSamples             int
	LimitQA                  int
	ExcludeQACategories      []int
	QATopK                   int
	QAGraphExpandedMaxSource int
	LimitTurns               int
	ReuseIngested            bool
	Concurrency              int
	QAConcurrency            int
	MaxSamples               int
	Hook                     EventHook
	ProgressPct              int
	PerCallTimeout           time.Duration
	ReportHook               ReportHook
}

func ParseTasks(spec string) ([]locomoreport.Task, error) {
	if strings.TrimSpace(spec) == "" {
		return []locomoreport.Task{locomoreport.TaskQA, locomoreport.TaskEvent, locomoreport.TaskDialog}, nil
	}
	var tasks []locomoreport.Task
	seen := map[locomoreport.Task]bool{}
	for _, raw := range strings.Split(spec, ",") {
		switch task := locomoreport.Task(strings.TrimSpace(strings.ToLower(raw))); task {
		case locomoreport.TaskQA, locomoreport.TaskEvent, locomoreport.TaskDialog:
			if !seen[task] {
				tasks = append(tasks, task)
				seen[task] = true
			}
		case "events", "summarization", "event_summarization":
			if !seen[locomoreport.TaskEvent] {
				tasks = append(tasks, locomoreport.TaskEvent)
				seen[locomoreport.TaskEvent] = true
			}
		case "caption_proxy", "multimodal", "mm":
			if !seen[locomoreport.TaskDialog] {
				tasks = append(tasks, locomoreport.TaskDialog)
				seen[locomoreport.TaskDialog] = true
			}
		default:
			return nil, fmt.Errorf("locomo: unknown task %q", raw)
		}
	}
	return tasks, nil
}

func Run(ctx context.Context, ds *dataset.Dataset, opts Options) (*locomoreport.Report, error) {
	if ds == nil {
		return nil, fmt.Errorf("locomo: dataset is required")
	}
	if opts.Memory == nil {
		return nil, fmt.Errorf("locomo: Options.Memory is required")
	}
	normalizeRunOptions(&opts)
	tasks := opts.Tasks
	if len(tasks) == 0 {
		tasks = []locomoreport.Task{locomoreport.TaskQA, locomoreport.TaskEvent, locomoreport.TaskDialog}
	}
	taskSet := taskSet(tasks)
	if opts.AnswerLLM == nil && (taskSet[locomoreport.TaskEvent] || taskSet[locomoreport.TaskDialog]) {
		return nil, fmt.Errorf("locomo: Options.AnswerLLM is required for event/dialog tasks")
	}
	if opts.AnswerLLM == nil && opts.JudgeLLM != nil {
		return nil, fmt.Errorf("locomo: Options.AnswerLLM is required when Options.JudgeLLM is set")
	}
	samples := limitedSamples(ds.Samples, opts.LimitSamples)

	rep := newReport(ds, opts, tasks, samples, taskSet)
	defer func() { rep.DurationMS = time.Since(rep.StartedAt).Milliseconds() }()
	emit := eventEmitter(ctx, opts.Hook)

	emit(Event{Kind: "start", Title: ds.Name, Body: fmt.Sprintf("LoCoMo memory eval: %d samples", len(samples))})
	log.Printf("[locomo] run start dataset=%s samples=%d tasks=%s limit_turns=%d limit_qa=%d per_call_timeout=%s", ds.Name, len(samples), taskListString(tasks), opts.LimitTurns, opts.LimitQA, opts.PerCallTimeout)
	if err := runSamples(ctx, rep, samples, taskSet, opts, emit); err != nil {
		return nil, err
	}
	locomoreport.Finalize(rep)
	emit(Event{Kind: "done", Title: ds.Name, Body: locomoreport.SummaryLine(rep)})
	log.Printf("[locomo] run done dataset=%s %s", ds.Name, locomoreport.SummaryLine(rep))
	return rep, nil
}

func normalizeRunOptions(opts *Options) {
	if opts.RunID == "" {
		opts.RunID = fmt.Sprintf("locomo-%d", time.Now().Unix())
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.QAConcurrency <= 0 {
		opts.QAConcurrency = 4
	}
	if opts.MaxSamples <= 0 {
		opts.MaxSamples = 200
	}
	if opts.QATopK <= 0 {
		opts.QATopK = defaultQATopK
	}
	if opts.QAGraphExpandedMaxSource < 0 {
		opts.QAGraphExpandedMaxSource = defaultQAGraphExpandedMaxSource
	}
}

func taskSet(tasks []locomoreport.Task) map[locomoreport.Task]bool {
	out := map[locomoreport.Task]bool{}
	for _, task := range tasks {
		out[task] = true
	}
	return out
}

func limitedSamples(samples []dataset.Sample, limit int) []dataset.Sample {
	if limit > 0 && len(samples) > limit {
		return samples[:limit]
	}
	return samples
}

func newReport(ds *dataset.Dataset, opts Options, tasks []locomoreport.Task, samples []dataset.Sample, tasksEnabled map[locomoreport.Task]bool) *locomoreport.Report {
	return locomoreport.New(ds.Name, opts.WorkspaceRoot, tasks, len(samples), map[string]any{
		"limit_samples":                opts.LimitSamples,
		"limit_qa":                     opts.LimitQA,
		"exclude_qa_categories":        append([]int(nil), opts.ExcludeQACategories...),
		"qa_top_k":                     opts.QATopK,
		"qa_graph_expanded_max_source": opts.QAGraphExpandedMaxSource,
		"limit_turns":                  opts.LimitTurns,
		"reuse_ingested":               opts.ReuseIngested,
		"run_id":                       opts.RunID,
		"concurrency":                  opts.Concurrency,
		"qa_concurrency":               opts.QAConcurrency,
		"n_samples":                    len(samples),
		"per_call_timeout_ms":          opts.PerCallTimeout.Milliseconds(),
		"judge_enabled":                opts.JudgeLLM != nil,
	}, tasksEnabled)
}

func eventEmitter(ctx context.Context, hook EventHook) func(Event) {
	return func(e Event) {
		if hook == nil {
			return
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		hook(ctx, e)
	}
}
