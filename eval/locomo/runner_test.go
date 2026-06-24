package locomo

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/localmem"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	locomosource "github.com/GizClaw/flowcraft/eval/locomo/source"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

type scriptedLLM struct {
	replies []string
	idx     int
}

func (s *scriptedLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if s.idx >= len(s.replies) {
		return model.NewTextMessage(model.RoleAssistant, ""), llm.TokenUsage{}, nil
	}
	reply := s.replies[s.idx]
	s.idx++
	return model.NewTextMessage(model.RoleAssistant, reply), llm.TokenUsage{}, nil
}

func (s *scriptedLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

type deadlineRecordingLLM struct {
	reply       string
	sawDeadline bool
}

func (d *deadlineRecordingLLM) Generate(ctx context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	_, d.sawDeadline = ctx.Deadline()
	return model.NewTextMessage(model.RoleAssistant, d.reply), llm.TokenUsage{}, nil
}

func (d *deadlineRecordingLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

type blockingLLM struct {
	reply      string
	started    chan struct{}
	release    <-chan struct{}
	onGenerate func(context.Context, []llm.Message)
}

func (b *blockingLLM) Generate(ctx context.Context, messages []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if b.onGenerate != nil {
		b.onGenerate(ctx, messages)
	}
	select {
	case b.started <- struct{}{}:
	case <-ctx.Done():
		return model.NewTextMessage(model.RoleAssistant, ""), llm.TokenUsage{}, ctx.Err()
	}
	select {
	case <-b.release:
		return model.NewTextMessage(model.RoleAssistant, b.reply), llm.TokenUsage{}, nil
	case <-ctx.Done():
		return model.NewTextMessage(model.RoleAssistant, ""), llm.TokenUsage{}, ctx.Err()
	}
}

func (b *blockingLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

type runResult struct {
	rep *locomoreport.Report
	err error
}

func TestRunAggregatesAllTasks(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{
		"Here is tea in the red mug.",
		"Ada likes tea.",
		"tea",
	}}
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		WorkspaceRoot: root,
		RunID:         "run-test",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA, locomoreport.TaskEvent, locomoreport.TaskDialog},
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.MemoryMode != locomoreport.MemoryModeLocalWorkspaceRawSource {
		t.Fatalf("memory mode = %q", rep.MemoryMode)
	}
	if rep.QAMetrics == nil || rep.QAMetrics.N != 1 || rep.QAMetrics.AverageF1 == 0 {
		t.Fatalf("qa metrics = %+v", rep.QAMetrics)
	}
	if rep.EventMetrics == nil || rep.EventMetrics.N != 1 || rep.EventMetrics.AverageRougeL == 0 {
		t.Fatalf("event metrics = %+v", rep.EventMetrics)
	}
	if rep.DialogMetrics == nil || rep.DialogMetrics.N != 1 || rep.DialogMetrics.TaskName != "caption_proxy_multimodal_dialog_generation" {
		t.Fatalf("dialog metrics = %+v", rep.DialogMetrics)
	}
	if got := len(rep.Samples[0].Dialog); got != 1 {
		t.Fatalf("dialog rows = %d, want 1", got)
	}
}

func TestRunQARetrievalOnlyWithoutAnswerLLM(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		WorkspaceRoot: root,
		RunID:         "run-retrieval-only",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.QAMetrics == nil || rep.QAMetrics.N != 1 {
		t.Fatalf("qa metrics = %+v, want one QA row", rep.QAMetrics)
	}
	if rep.QAMetrics.EvidenceRecallAtK != 1 {
		t.Fatalf("evidence recall = %.3f, want 1", rep.QAMetrics.EvidenceRecallAtK)
	}
	row := rep.Samples[0].QA[0]
	if row.Predicted != "" || row.F1 != 0 || row.Judge != nil {
		t.Fatalf("retrieval-only row predicted=%q f1=%.3f judge=%+v, want no answer fields", row.Predicted, row.F1, row.Judge)
	}
	if row.HitCounts == nil || row.HitCounts.SourceMessages == 0 {
		t.Fatalf("hit counts = %+v, want retrieval context counts", row.HitCounts)
	}
}

func TestRunRequiresAnswerLLMForEventDialogAndJudge(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	if _, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		WorkspaceRoot: root,
		RunID:         "run-event-needs-answer",
		Tasks:         []locomoreport.Task{locomoreport.TaskEvent},
	}); err == nil || !strings.Contains(err.Error(), "AnswerLLM is required for event/dialog") {
		t.Fatalf("event without answer error = %v, want answer requirement", err)
	}
	if _, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		JudgeLLM:      &scriptedLLM{},
		WorkspaceRoot: root,
		RunID:         "run-judge-needs-answer",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
	}); err == nil || !strings.Contains(err.Error(), "AnswerLLM is required when Options.JudgeLLM is set") {
		t.Fatalf("judge without answer error = %v, want answer requirement", err)
	}
}

func TestRunAppliesAndReportsPerCallTimeout(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &deadlineRecordingLLM{reply: "tea"}
	rep, err := Run(context.Background(), ds, Options{
		Memory:         mem,
		AnswerLLM:      answer,
		WorkspaceRoot:  root,
		RunID:          "run-timeout",
		Tasks:          []locomoreport.Task{locomoreport.TaskQA},
		LimitSamples:   1,
		LimitQA:        1,
		Concurrency:    1,
		PerCallTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !answer.sawDeadline {
		t.Fatal("answer LLM context had no deadline")
	}
	if got := rep.Options["per_call_timeout_ms"]; got != int64(50) {
		t.Fatalf("per_call_timeout_ms = %v, want 50", got)
	}
	if got := rep.Options["qa_top_k"]; got != defaultQATopK {
		t.Fatalf("qa_top_k = %v, want default %d", got, defaultQATopK)
	}
}

func TestRunReportsCustomQATopK(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{"tea"}}
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		WorkspaceRoot: root,
		RunID:         "run-custom-top-k",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitSamples:  1,
		LimitQA:       1,
		QATopK:        7,
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rep.Options["qa_top_k"]; got != 7 {
		t.Fatalf("qa_top_k = %v, want custom 7", got)
	}
}

func TestRunReportsDefaultConcurrencies(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{"tea"}}
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		WorkspaceRoot: root,
		RunID:         "run-default-concurrency",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitSamples:  1,
		LimitQA:       1,
		Concurrency:   0,
		QAConcurrency: 0,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rep.Options["concurrency"]; got != 4 {
		t.Fatalf("concurrency = %v, want default 4", got)
	}
	if got := rep.Options["qa_concurrency"]; got != 4 {
		t.Fatalf("qa_concurrency = %v, want default 4", got)
	}
}

func TestRunSamplesCanRunQAStageConcurrently(t *testing.T) {
	ds := mustSyntheticDataset(t)
	second := ds.Samples[0]
	second.ID = "conv-b"
	ds.Samples = []dataset.Sample{ds.Samples[0], second}
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()

	release := make(chan struct{})
	answer := &blockingLLM{
		reply:   "tea",
		started: make(chan struct{}, 2),
		release: release,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan runResult, 1)
	go func() {
		rep, err := Run(ctx, ds, Options{
			Memory:        mem,
			AnswerLLM:     answer,
			WorkspaceRoot: root,
			RunID:         "run-sample-concurrency",
			Tasks:         []locomoreport.Task{locomoreport.TaskQA},
			LimitQA:       1,
			Concurrency:   2,
			QAConcurrency: 1,
		})
		done <- runResult{rep: rep, err: err}
	}()

	waitForStarts(t, answer.started, 2)
	close(release)
	result := waitForRun(t, done)
	if result.err != nil {
		t.Fatalf("Run: %v", result.err)
	}
	if got := sampleIDs(result.rep.Samples); strings.Join(got, ",") != "conv-a,conv-b" {
		t.Fatalf("sample order = %v, want [conv-a conv-b]", got)
	}
}

func TestRunQAItemsStartAfterIngestAndCanRunConcurrently(t *testing.T) {
	ds := mustSyntheticDataset(t)
	ds.Samples[0].QA = []dataset.QAItem{
		{ID: "qa-1", Question: "What does Ada like?", Answer: "tea", CategoryID: 1, Evidence: []string{"d1"}},
		{ID: "qa-2", Question: "What is in the mug?", Answer: "tea", CategoryID: 4, Evidence: []string{"d3"}},
		{ID: "qa-3", Question: "Who thanked Ada?", Answer: "Ben", CategoryID: 4, Evidence: []string{"d4"}},
	}
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()

	scope := locomosource.SampleScope("run-qa-concurrency", ds.Name, ds.Samples[0])
	wantMessages := 0
	for _, session := range ds.Samples[0].Sessions {
		wantMessages += len(session.Turns)
	}
	var mu sync.Mutex
	var seenCounts []int
	release := make(chan struct{})
	answer := &blockingLLM{
		reply:   "tea",
		started: make(chan struct{}, len(ds.Samples[0].QA)),
		release: release,
		onGenerate: func(ctx context.Context, _ []llm.Message) {
			messages, err := mem.MessageStore().List(ctx, scope.ConversationID, sourcemessage.ListOptions{})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				seenCounts = append(seenCounts, -1)
				return
			}
			seenCounts = append(seenCounts, len(messages))
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan runResult, 1)
	go func() {
		rep, err := Run(ctx, ds, Options{
			Memory:        mem,
			AnswerLLM:     answer,
			WorkspaceRoot: root,
			RunID:         "run-qa-concurrency",
			Tasks:         []locomoreport.Task{locomoreport.TaskQA},
			LimitSamples:  1,
			Concurrency:   1,
			QAConcurrency: 3,
		})
		done <- runResult{rep: rep, err: err}
	}()

	waitForStarts(t, answer.started, 3)
	mu.Lock()
	counts := append([]int(nil), seenCounts...)
	mu.Unlock()
	if len(counts) != 3 {
		t.Fatalf("answer calls observed = %d, want 3", len(counts))
	}
	for _, count := range counts {
		if count != wantMessages {
			cancel()
			close(release)
			t.Fatalf("messages visible at QA start = %d, want all %d", count, wantMessages)
		}
	}
	close(release)
	result := waitForRun(t, done)
	if result.err != nil {
		t.Fatalf("Run: %v", result.err)
	}
	if got := len(result.rep.Samples[0].QA); got != 3 {
		t.Fatalf("QA rows = %d, want 3", got)
	}
	if got := result.rep.Samples[0].QA[0].ID; got != "qa-1" {
		t.Fatalf("first QA ID = %q, want qa-1", got)
	}
}

func TestSelectedQAItemsExcludesCategoriesBeforeLimit(t *testing.T) {
	qas := []dataset.QAItem{
		{ID: "q1", CategoryID: 5},
		{ID: "q2", CategoryID: 1},
		{ID: "q3", CategoryID: 5},
		{ID: "q4", CategoryID: 2},
		{ID: "q5", CategoryID: 3},
	}

	got := selectedQAItems(qas, 2, []int{5})
	if len(got) != 2 || got[0].ID != "q2" || got[1].ID != "q4" {
		t.Fatalf("selected QA = %+v, want q2/q4 after excluding category 5 before limit", got)
	}
}

func TestRunAppliesQAJudgeMetrics(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{"tea"}}
	judge := &scriptedLLM{replies: []string{`{"verdict":"correct","rationale":"matches gold"}`}}
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		JudgeLLM:      judge,
		WorkspaceRoot: root,
		RunID:         "run-judge",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitSamples:  1,
		LimitTurns:    1,
		LimitQA:       1,
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.QAMetrics == nil || rep.QAMetrics.JudgeEvaluatedCount != 1 || rep.QAMetrics.JudgeAccuracy != 1 {
		t.Fatalf("judge metrics = %+v, want one correct judged QA", rep.QAMetrics)
	}
	row := rep.Samples[0].QA[0]
	if row.Judge == nil || row.Judge.Verdict != "correct" || !row.Judge.Correct {
		t.Fatalf("row judge = %+v, want correct", row.Judge)
	}
	if rep.Samples[0].QAMetrics == nil || rep.Samples[0].QAMetrics.JudgeCorrectCount != 1 {
		t.Fatalf("sample judge metrics = %+v, want correct count", rep.Samples[0].QAMetrics)
	}
}

func TestRunWritesProgressReportSnapshots(t *testing.T) {
	ds := mustSyntheticDataset(t)
	second := ds.Samples[0]
	second.ID = "conv-b"
	ds.Samples = append(ds.Samples, second)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{"tea", "tea"}}
	var snapshots []*locomoreport.Report
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		WorkspaceRoot: root,
		RunID:         "run-progress-report",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitTurns:    1,
		LimitQA:       1,
		Concurrency:   1,
		ReportHook: func(_ context.Context, rep *locomoreport.Report) error {
			snapshots = append(snapshots, rep)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(snapshots); got != 2 {
		t.Fatalf("progress snapshots = %d, want 2", got)
	}
	first := snapshots[0]
	if !first.Partial || first.CompletedSamples != 1 || first.TotalSamples != 2 {
		t.Fatalf("first snapshot progress = partial:%t completed:%d total:%d, want partial 1/2", first.Partial, first.CompletedSamples, first.TotalSamples)
	}
	if len(first.Samples) != 1 || first.Samples[0].QAMetrics == nil || first.Samples[0].QAMetrics.N != 1 {
		t.Fatalf("first snapshot samples = %+v, want first sample QA metrics", first.Samples)
	}
	if first.Samples[0].QAMetrics.AverageF1 != 1 {
		t.Fatalf("first sample AverageF1 = %.3f, want 1", first.Samples[0].QAMetrics.AverageF1)
	}
	if first.QAMetrics == nil || first.QAMetrics.N != 1 || first.QAMetrics.AverageF1 != 1 {
		t.Fatalf("first snapshot aggregate QA metrics = %+v, want one finalized QA", first.QAMetrics)
	}
	secondSnap := snapshots[1]
	if secondSnap.Partial || secondSnap.CompletedSamples != 2 || secondSnap.TotalSamples != 2 {
		t.Fatalf("second snapshot progress = partial:%t completed:%d total:%d, want complete 2/2", secondSnap.Partial, secondSnap.CompletedSamples, secondSnap.TotalSamples)
	}
	if rep.QAMetrics == nil || rep.QAMetrics.N != 2 || rep.QAMetrics.AverageF1 != 1 {
		t.Fatalf("final report QA metrics = %+v, want two finalized QA", rep.QAMetrics)
	}
}

func TestRunHonorsLimitTurns(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{"tea"}}
	rep, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		WorkspaceRoot: root,
		RunID:         "run-limit-turns",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitTurns:    1,
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Options["limit_turns"] != 1 {
		t.Fatalf("report limit_turns = %v, want 1", rep.Options["limit_turns"])
	}
	scope := locomosource.SampleScope("run-limit-turns", ds.Name, ds.Samples[0])
	messages, err := mem.MessageStore().List(context.Background(), scope.ConversationID, sourcemessage.ListOptions{})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	if got := len(messages); got != 1 {
		t.Fatalf("ingested messages = %d, want 1", got)
	}
}

func TestRunBatchesSourceMessageIngestWhenDialogIsDisabled(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	answer := &scriptedLLM{replies: []string{"tea"}}
	_, err = Run(context.Background(), ds, Options{
		Memory:        mem,
		AnswerLLM:     answer,
		WorkspaceRoot: root,
		RunID:         "run-batch-session",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitSamples:  1,
		LimitTurns:    3,
		LimitQA:       1,
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	scope := locomosource.SampleScope("run-batch-session", ds.Name, ds.Samples[0])
	messages, err := mem.MessageStore().List(context.Background(), scope.ConversationID, sourcemessage.ListOptions{})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("ingested source messages = %d, want one 3-turn session batch", len(messages))
	}
}

func TestRunReusesCompleteIngestForStableRunID(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	first, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		WorkspaceRoot: root,
		RunID:         "run-reuse",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	if first.QAMetrics == nil || first.QAMetrics.N != 1 {
		t.Fatalf("initial QA metrics = %+v, want one QA row", first.QAMetrics)
	}
	scope := locomosource.SampleScope("run-reuse", ds.Name, ds.Samples[0])
	messages, err := mem.MessageStore().List(context.Background(), scope.ConversationID, sourcemessage.ListOptions{})
	if err != nil {
		t.Fatalf("List messages: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("initial ingested messages = %d, want full synthetic sample", len(messages))
	}
	second, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		WorkspaceRoot: root,
		RunID:         "run-reuse",
		ReuseIngested: true,
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		Concurrency:   1,
	})
	if err != nil {
		t.Fatalf("reuse Run: %v", err)
	}
	if second.QAMetrics == nil || second.QAMetrics.N != 1 {
		t.Fatalf("reuse QA metrics = %+v, want one QA row", second.QAMetrics)
	}
	messages, err = mem.MessageStore().List(context.Background(), scope.ConversationID, sourcemessage.ListOptions{})
	if err != nil {
		t.Fatalf("List messages after reuse: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages after reuse = %d, want unchanged full synthetic sample", len(messages))
	}
	if got := second.Options["reuse_ingested"]; got != true {
		t.Fatalf("reuse_ingested option = %v, want true", got)
	}
}

func TestRunRejectsPartialReusableIngest(t *testing.T) {
	ds := mustSyntheticDataset(t)
	root := t.TempDir()
	mem, closeMem, err := localmem.Build(localmem.MemoryOptions{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("BuildLocalMemory: %v", err)
	}
	defer func() { _ = closeMem() }()
	if _, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		WorkspaceRoot: root,
		RunID:         "run-partial-reuse",
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		LimitTurns:    1,
		Concurrency:   1,
	}); err != nil {
		t.Fatalf("partial initial Run: %v", err)
	}
	if _, err := Run(context.Background(), ds, Options{
		Memory:        mem,
		WorkspaceRoot: root,
		RunID:         "run-partial-reuse",
		ReuseIngested: true,
		Tasks:         []locomoreport.Task{locomoreport.TaskQA},
		Concurrency:   1,
	}); err == nil || !strings.Contains(err.Error(), "partial ingest") {
		t.Fatalf("reuse partial error = %v, want partial ingest error", err)
	}
}

func TestParseTasks(t *testing.T) {
	tasks, err := ParseTasks("qa,events,caption_proxy")
	if err != nil {
		t.Fatalf("ParseTasks: %v", err)
	}
	want := []locomoreport.Task{locomoreport.TaskQA, locomoreport.TaskEvent, locomoreport.TaskDialog}
	if len(tasks) != len(want) {
		t.Fatalf("tasks = %v, want %v", tasks, want)
	}
	for i := range want {
		if tasks[i] != want[i] {
			t.Fatalf("tasks = %v, want %v", tasks, want)
		}
	}
	if _, err := ParseTasks("qa,nope"); err == nil {
		t.Fatal("ParseTasks invalid task error = nil")
	}
}

func waitForStarts(t *testing.T, started <-chan struct{}, want int) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for i := 0; i < want; i++ {
		select {
		case <-started:
		case <-timer.C:
			t.Fatalf("LLM calls started = %d, want %d", i, want)
		}
	}
}

func waitForRun(t *testing.T, done <-chan runResult) runResult {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case result := <-done:
		return result
	case <-timer.C:
		t.Fatal("Run did not finish")
		return runResult{}
	}
}

func sampleIDs(samples []locomoreport.SampleResult) []string {
	ids := make([]string, 0, len(samples))
	for _, sample := range samples {
		ids = append(ids, sample.ID)
	}
	return ids
}

func mustSyntheticDataset(t *testing.T) *dataset.Dataset {
	t.Helper()
	ds, err := dataset.Decode([]byte(syntheticJSON()))
	if err != nil {
		t.Fatalf("dataset.Decode: %v", err)
	}
	ds.Name = "synthetic"
	return ds
}

func syntheticJSON() string {
	return `{
  "samples": [
    {
      "sample_id": "conv-a",
      "conversation": {
        "speaker_1": "Ada",
        "speaker_2": "Ben",
        "session_1_date_time": "2024-01-01 09:00",
        "session_1": [
          {"dia_id": "d1", "speaker": "Ada", "text": "Ada likes tea."},
          {"dia_id": "d2", "speaker": "Ben", "text": "What is in the image?", "img_url": ["https://example.test/mug.png"], "blip_caption": "a red mug on a table", "query": "What is shown?"},
          {"dia_id": "d3", "speaker": "Ada", "text": "Here is tea in the red mug."}
        ],
        "session_2_date_time": "2024-01-02 09:00",
        "session_2": [
          {"dia_id": "d4", "speaker": "Ben", "text": "Thanks."}
        ]
      },
      "qa": [
        {"question": "What does Ada like?", "answer": "tea", "category": 1, "evidence": ["d1"], "adversarial_answer": "no information available"}
      ],
      "event_summary": {
        "events_session_1": {"Ada": ["Ada likes tea."]}
      }
    }
  ]
}`
}

func TestSummaryLineUsesEnabledTasks(t *testing.T) {
	if got := locomoreport.SummaryLine(&locomoreport.Report{QAMetrics: &locomoreport.QAMetrics{AverageF1: 1, EvidenceRecallAtK: 0.5}}); !strings.Contains(got, "qa_f1=1.000") || !strings.Contains(got, "evidence_recall_at_k=0.500") {
		t.Fatalf("summaryLine = %q, want QA metric", got)
	}
}
