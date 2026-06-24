package locomo

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	locomosource "github.com/GizClaw/flowcraft/eval/locomo/source"
	"github.com/GizClaw/flowcraft/eval/locomo/tasks"
	"github.com/GizClaw/flowcraft/memory"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
)

type sampleJob struct {
	index  int
	sample dataset.Sample
}

type sampleRunResult struct {
	index  int
	sample dataset.Sample
	result locomoreport.SampleResult
	err    error
}

type qaJob struct {
	index int
	item  dataset.QAItem
}

type qaRunResult struct {
	index int
	row   locomoreport.QAResult
}

func runSamples(ctx context.Context, rep *locomoreport.Report, samples []dataset.Sample, taskSet map[locomoreport.Task]bool, opts Options, emit func(Event)) error {
	if len(samples) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workerCount := minPositive(opts.Concurrency, len(samples))
	jobs := make(chan sampleJob)
	results := make(chan sampleRunResult, len(samples))
	var wg sync.WaitGroup
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if err := ctx.Err(); err != nil {
					return
				}
				result, err := runSample(ctx, rep.Dataset, job.index, len(samples), job.sample, taskSet, opts)
				results <- sampleRunResult{index: job.index, sample: job.sample, result: result, err: err}
				if err != nil {
					cancel()
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for sampleIdx, sample := range samples {
			select {
			case <-ctx.Done():
				return
			case jobs <- sampleJob{index: sampleIdx, sample: sample}:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	nextProgressPct := opts.ProgressPct
	sampleResults := make([]*locomoreport.SampleResult, len(samples))
	completed := 0
	var firstErr error
	for runResult := range results {
		if runResult.err != nil {
			if firstErr == nil {
				firstErr = runResult.err
				cancel()
			}
			continue
		}
		if firstErr != nil {
			continue
		}
		aggregateSampleResult(rep, runResult.sample, runResult.result, taskSet, opts)
		sampleResult := runResult.result
		sampleResults[runResult.index] = &sampleResult
		rep.Samples = orderedCompletedSamples(sampleResults, opts.MaxSamples)
		completed++
		rep.CompletedSamples = completed
		if opts.ProgressPct > 0 && len(samples) > 0 {
			pct := completed * 100 / len(samples)
			if pct >= nextProgressPct && pct < 100 {
				emit(Event{
					Kind: "locomo_progress",
					Body: fmt.Sprintf("%d/%d (~%d%%)", completed, len(samples), pct),
					Fields: map[string]string{
						"done":  fmt.Sprintf("%d", completed),
						"total": fmt.Sprintf("%d", len(samples)),
						"pct":   fmt.Sprintf("%d", pct),
					},
				})
				nextProgressPct += opts.ProgressPct
			}
		}
		if opts.ReportHook != nil {
			if err := opts.ReportHook(ctx, locomoreport.Snapshot(rep, rep.CompletedSamples < rep.TotalSamples)); err != nil {
				return fmt.Errorf("write progress report: %w", err)
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func runSample(ctx context.Context, datasetName string, sampleIdx, totalSamples int, sample dataset.Sample, taskSet map[locomoreport.Task]bool, opts Options) (locomoreport.SampleResult, error) {
	scope := locomosource.SampleScope(opts.RunID, datasetName, sample)
	sampleResult := locomoreport.SampleResult{ID: sample.ID}
	if taskSet[locomoreport.TaskQA] {
		sampleResult.QAMetrics = &locomoreport.QAMetrics{}
	}
	ingestedTurns := 0
	limitReached := false
	log.Printf("[locomo] sample start %d/%d id=%s sessions=%d qa=%d", sampleIdx+1, totalSamples, sample.ID, len(sample.Sessions), len(sample.QA))
	reuseIngested := false
	if opts.ReuseIngested {
		status, err := determineSampleIngestStatus(ctx, opts.Memory, scope, sample, opts.LimitTurns)
		if err != nil {
			return sampleResult, err
		}
		switch status {
		case sampleIngestComplete:
			reuseIngested = true
			ingestedTurns = expectedSampleTurnCount(sample, opts.LimitTurns)
			log.Printf("[locomo] sample reuse-ingest id=%s run_id=%s turns=%d", sample.ID, opts.RunID, ingestedTurns)
		case sampleIngestPartial:
			return sampleResult, fmt.Errorf("locomo: workspace has partial ingest for sample %s run_id=%s; use a fresh run-id/workspace or complete the ingest first", sample.ID, opts.RunID)
		}
	}
	if !reuseIngested {
		for _, session := range sample.Sessions {
			sessionIngested := false
			log.Printf("[locomo] session ingest start sample=%s session=%d turns=%d", sample.ID, session.Index, len(session.Turns))
			if taskSet[locomoreport.TaskDialog] {
				for _, turn := range session.Turns {
					if opts.LimitTurns > 0 && ingestedTurns >= opts.LimitTurns {
						limitReached = true
						break
					}
					if err := locomosource.IngestTurns(ctx, opts.Memory, scope, session, []dataset.Turn{turn}); err != nil {
						return sampleResult, fmt.Errorf("ingest sample %s session %d turn %s: %w", sample.ID, session.Index, turn.DiaID, err)
					}
					ingestedTurns++
					sessionIngested = true
					if c, ok := tasks.DialogCaseForTurn(sample.DialogCases, session.Index, turn.DiaID); ok {
						row := tasks.RunDialog(ctx, opts.Memory, opts.AnswerLLM, scope, c, opts.PerCallTimeout)
						sampleResult.Dialog = append(sampleResult.Dialog, row)
					}
				}
			} else {
				turns := sessionTurnsWithinLimit(session.Turns, opts.LimitTurns, ingestedTurns)
				if len(turns) == 0 {
					if opts.LimitTurns > 0 && ingestedTurns >= opts.LimitTurns {
						limitReached = true
					}
					break
				}
				batch := session
				batch.Turns = turns
				if err := locomosource.IngestSession(ctx, opts.Memory, scope, batch); err != nil {
					return sampleResult, fmt.Errorf("ingest sample %s session %d: %w", sample.ID, session.Index, err)
				}
				ingestedTurns += len(turns)
				sessionIngested = true
				if opts.LimitTurns > 0 && ingestedTurns >= opts.LimitTurns {
					limitReached = true
				}
			}
			log.Printf("[locomo] session ingest end sample=%s session=%d ingested=%t total_turns=%d", sample.ID, session.Index, sessionIngested, ingestedTurns)
			if taskSet[locomoreport.TaskEvent] && sessionIngested {
				for _, target := range tasks.EventsForSession(sample.EventSummaries, session.Index) {
					row := tasks.RunEvent(ctx, opts.Memory, opts.AnswerLLM, scope, target, opts.PerCallTimeout)
					sampleResult.Events = append(sampleResult.Events, row)
				}
			}
			if limitReached {
				break
			}
		}
	}
	if taskSet[locomoreport.TaskQA] {
		qas := selectedQAItems(sample.QA, opts.LimitQA, opts.ExcludeQACategories)
		rows, err := runQAItems(ctx, scope, sample.ID, qas, opts)
		if err != nil {
			return sampleResult, err
		}
		sampleResult.QA = rows
		for qaIdx, item := range qas {
			locomoreport.AccumulateQA(sampleResult.QAMetrics, item, sampleResult.QA[qaIdx])
		}
	}
	locomoreport.FinalizeQA(sampleResult.QAMetrics)
	log.Printf("[locomo] sample end %d/%d id=%s qa=%d events=%d dialog=%d", sampleIdx+1, totalSamples, sample.ID, len(sampleResult.QA), len(sampleResult.Events), len(sampleResult.Dialog))
	return sampleResult, nil
}

type sampleIngestStatus int

const (
	sampleIngestMissing sampleIngestStatus = iota
	sampleIngestPartial
	sampleIngestComplete
)

func determineSampleIngestStatus(ctx context.Context, mem *memory.System, scope memory.Scope, sample dataset.Sample, limitTurns int) (sampleIngestStatus, error) {
	store := mem.MessageStore()
	if store == nil {
		return sampleIngestMissing, fmt.Errorf("locomo: ingest reuse requires message store")
	}
	messages, err := store.List(ctx, scope.ConversationID, sourcemessage.ListOptions{})
	if err != nil {
		return sampleIngestMissing, err
	}
	if len(messages) == 0 {
		return sampleIngestMissing, nil
	}
	expected := expectedSampleDiaIDs(sample, limitTurns)
	if len(expected) == 0 {
		return sampleIngestComplete, nil
	}
	seen := map[string]bool{}
	for _, msg := range messages {
		seen[msg.ID] = true
	}
	matched := 0
	for id := range expected {
		if seen[id] {
			matched++
		}
	}
	switch {
	case matched == 0:
		return sampleIngestMissing, nil
	case matched == len(expected):
		return sampleIngestComplete, nil
	default:
		return sampleIngestPartial, nil
	}
}

func expectedSampleDiaIDs(sample dataset.Sample, limitTurns int) map[string]bool {
	ids := map[string]bool{}
	count := 0
	for _, session := range sample.Sessions {
		for _, turn := range session.Turns {
			if limitTurns > 0 && count >= limitTurns {
				return ids
			}
			ids[turn.DiaID] = true
			count++
		}
	}
	return ids
}

func expectedSampleTurnCount(sample dataset.Sample, limitTurns int) int {
	count := 0
	for _, session := range sample.Sessions {
		for range session.Turns {
			if limitTurns > 0 && count >= limitTurns {
				return count
			}
			count++
		}
	}
	return count
}

func runQAItems(ctx context.Context, scope memory.Scope, sampleID string, qas []dataset.QAItem, opts Options) ([]locomoreport.QAResult, error) {
	if len(qas) == 0 {
		return nil, nil
	}
	workerCount := minPositive(opts.QAConcurrency, len(qas))
	jobs := make(chan qaJob)
	results := make(chan qaRunResult, len(qas))
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if shouldLogQAProgress(job.index, len(qas)) {
					log.Printf("[locomo] qa progress sample=%s item=%d/%d id=%s", sampleID, job.index+1, len(qas), job.item.ID)
				}
				row := tasks.RunQAWithOptions(ctx, opts.Memory, opts.AnswerLLM, opts.JudgeLLM, scope, job.item, tasks.QARetrievalOptions{
					TopK:                   opts.QATopK,
					GraphExpandedMaxSource: opts.QAGraphExpandedMaxSource,
				}, opts.PerCallTimeout)
				results <- qaRunResult{index: job.index, row: row}
				if ctx.Err() != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for qaIdx, item := range qas {
			select {
			case <-ctx.Done():
				return
			case jobs <- qaJob{index: qaIdx, item: item}:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	rows := make([]locomoreport.QAResult, len(qas))
	received := 0
	for result := range results {
		rows[result.index] = result.row
		received++
	}
	if err := ctx.Err(); err != nil {
		return rows[:received], err
	}
	return rows, nil
}

func aggregateSampleResult(rep *locomoreport.Report, sample dataset.Sample, sampleResult locomoreport.SampleResult, taskSet map[locomoreport.Task]bool, opts Options) {
	if taskSet[locomoreport.TaskQA] {
		qas := selectedQAItems(sample.QA, opts.LimitQA, opts.ExcludeQACategories)
		for qaIdx, row := range sampleResult.QA {
			if qaIdx >= len(qas) {
				break
			}
			locomoreport.AccumulateQA(rep.QAMetrics, qas[qaIdx], row)
		}
	}
	for _, row := range sampleResult.Events {
		locomoreport.AccumulateEvent(rep.EventMetrics, row)
	}
	for _, row := range sampleResult.Dialog {
		locomoreport.AccumulateDialog(rep.DialogMetrics, row)
	}
}

func orderedCompletedSamples(sampleResults []*locomoreport.SampleResult, maxSamples int) []locomoreport.SampleResult {
	if maxSamples <= 0 {
		return nil
	}
	limit := minPositive(maxSamples, len(sampleResults))
	out := make([]locomoreport.SampleResult, 0, limit)
	for _, sampleResult := range sampleResults[:limit] {
		if sampleResult == nil {
			continue
		}
		out = append(out, *sampleResult)
	}
	return out
}

func selectedQAItems(qas []dataset.QAItem, limit int, excludedCategories []int) []dataset.QAItem {
	filtered := filterQAItemsByCategory(qas, excludedCategories)
	if limit > 0 && len(filtered) > limit {
		return filtered[:limit]
	}
	return filtered
}

func filterQAItemsByCategory(qas []dataset.QAItem, excludedCategories []int) []dataset.QAItem {
	if len(qas) == 0 || len(excludedCategories) == 0 {
		return qas
	}
	excluded := map[int]bool{}
	for _, categoryID := range excludedCategories {
		excluded[categoryID] = true
	}
	out := make([]dataset.QAItem, 0, len(qas))
	for _, item := range qas {
		if excluded[item.CategoryID] {
			continue
		}
		out = append(out, item)
	}
	return out
}

func minPositive(a, b int) int {
	if a <= 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

func sessionTurnsWithinLimit(turns []dataset.Turn, limit, already int) []dataset.Turn {
	if limit <= 0 {
		return turns
	}
	remaining := limit - already
	if remaining <= 0 {
		return nil
	}
	if len(turns) <= remaining {
		return turns
	}
	return turns[:remaining]
}

func shouldLogQAProgress(index, total int) bool {
	return index == 0 || index+1 == total || (index+1)%10 == 0
}

func taskListString(tasks []locomoreport.Task) string {
	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		parts = append(parts, string(task))
	}
	return strings.Join(parts, ",")
}
