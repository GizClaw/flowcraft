package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	memoryconfig "github.com/GizClaw/flowcraft/memory/config"
	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/memory/eval/scenarios"
	"github.com/GizClaw/flowcraft/sdk/inference"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type runOptions struct {
	Dataset       *dataset.Dataset
	InferencePath string
	Scenario      scenarios.Scenario
	GenerateModel string
	EmbedModel    string
	AnswerModel   string
	JudgeModel    string
	MaxItems      int
	MaxTokens     int
	Limit         int
	Concurrency   int
	IngestTimeout time.Duration
	QATimeout     time.Duration
	Notifier      notifier
	ProgressPct   int
}

// Run ingests the dataset through a real memory assembly and evaluates every
// question with the sdk-default retrieval profile.
func Run(ctx context.Context, opts runOptions) (*Report, error) {
	notifier := opts.Notifier
	if notifier == nil {
		notifier = noopNotifier{}
	}
	host, _ := os.Hostname()
	bestEffortNotify(ctx, notifier, notifyEvent{
		Kind:   "start",
		Title:  "memory eval started",
		Fields: map[string]string{"host": host},
	})
	report, err := runInternal(ctx, opts, notifier)
	if err != nil {
		bestEffortNotify(ctx, notifier, notifyEvent{
			Kind: "error", Title: "memory eval failed", Body: err.Error(),
		})
		return nil, err
	}
	bestEffortNotify(ctx, notifier, notifyEvent{
		Kind: "done", Title: "memory eval finished", Body: reportSummary(report),
	})
	return report, nil
}

func runInternal(ctx context.Context, opts runOptions, n notifier) (*Report, error) {
	if opts.Dataset == nil {
		return nil, errors.New("eval: dataset is required")
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = defaultMaxItems
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = defaultMaxTokens
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	generateRef, err := parseModelRef(opts.GenerateModel)
	if err != nil {
		return nil, fmt.Errorf("--generate-model: %w", err)
	}
	embedRef, err := parseModelRef(opts.EmbedModel)
	if err != nil {
		return nil, fmt.Errorf("--embed-model: %w", err)
	}
	answerRef, err := parseModelRef(opts.AnswerModel)
	if err != nil {
		return nil, fmt.Errorf("--answer-model: %w", err)
	}
	var judgeRef *inference.ModelRef
	if opts.JudgeModel != "" {
		ref, refErr := parseModelRef(opts.JudgeModel)
		if refErr != nil {
			return nil, fmt.Errorf("--judge-model: %w", refErr)
		}
		judgeRef = &ref
	}

	runtime, err := buildInferenceRuntime(ctx, opts.InferencePath)
	if err != nil {
		return nil, err
	}
	ws := workspace.NewMemWorkspace()
	builder, err := memoryconfig.NewBuilder(ws, runtime)
	if err != nil {
		return nil, fmt.Errorf("memory builder: %w", err)
	}
	scopes := make([]memoryconfig.ScopeSettings, 0, len(opts.Dataset.Conversations))
	for _, conversation := range opts.Dataset.Conversations {
		scope := scopeFor(opts.Scenario.RuntimeID(), conversation.ID)
		scopes = append(scopes, memoryconfig.ScopeSettings{
			RuntimeID: scope.RuntimeID,
			UserID:    scope.UserID,
			AgentID:   scope.AgentID,
		})
	}
	assembly, err := builder.NewAssembly(ctx, memoryconfig.Settings{
		Generate:  toModelSettings(generateRef),
		Embed:     toModelSettings(embedRef),
		Scopes:    scopes,
		Interval:  memoryconfig.Duration(time.Hour),
		Lifecycle: memoryconfig.LifecycleSettings{Disabled: true},
	})
	if err != nil {
		return nil, fmt.Errorf("memory assembly: %w", err)
	}
	defer assembly.Close()

	started := time.Now().UTC()
	seqByEvidence := make(map[string]map[string]uint64)
	for index, conversation := range opts.Dataset.Conversations {
		if err := ingestConversation(
			ctx, assembly, opts.Scenario, conversation, seqByEvidence, opts.IngestTimeout,
		); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "ingest %d/%d conversations\n", index+1, len(opts.Dataset.Conversations))
	}
	for index, conversation := range opts.Dataset.Conversations {
		deriveCtx := ctx
		var cancel context.CancelFunc
		if opts.IngestTimeout > 0 {
			deriveCtx, cancel = context.WithTimeout(ctx, opts.IngestTimeout)
		}
		err := assembly.Runner.ProcessScope(deriveCtx, scopeFor(opts.Scenario.RuntimeID(), conversation.ID))
		if cancel != nil {
			cancel()
		}
		if err != nil {
			// Transient LLM failures (bad JSON, provider blips) abort one
			// node; the durable DAG checkpoint makes the retry cheap by
			// re-running only failed nodes.
			retryCtx := ctx
			var retryCancel context.CancelFunc
			if opts.IngestTimeout > 0 {
				retryCtx, retryCancel = context.WithTimeout(ctx, opts.IngestTimeout)
			}
			retryErr := assembly.Runner.ProcessScope(retryCtx, scopeFor(opts.Scenario.RuntimeID(), conversation.ID))
			if retryCancel != nil {
				retryCancel()
			}
			if retryErr != nil {
				return nil, fmt.Errorf("derive conversation %s: %w", conversation.ID, retryErr)
			}
			fmt.Fprintf(os.Stderr, "derive %d/%d conversations (retried)\n", index+1, len(opts.Dataset.Conversations))
			continue
		}
		fmt.Fprintf(os.Stderr, "derive %d/%d conversations\n", index+1, len(opts.Dataset.Conversations))
	}
	bestEffortNotify(ctx, n, notifyEvent{
		Kind:  "ingest_done",
		Title: fmt.Sprintf("ingested %d conversations", len(opts.Dataset.Conversations)),
	})

	questions := opts.Dataset.Questions
	if opts.Limit > 0 && len(questions) > opts.Limit {
		questions = questions[:opts.Limit]
	}
	scores := make([]scenarios.QuestionScore, len(questions))
	latency := newLatencyAggregator()
	var (
		wg            sync.WaitGroup
		mu            sync.Mutex
		sem           = make(chan struct{}, opts.Concurrency)
		completed     int
		nextMilestone = opts.ProgressPct
		milestoneMu   sync.Mutex
	)
	for index, question := range questions {
		wg.Add(1)
		go func(index int, question dataset.Question) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			score := evalQuestion(
				ctx, opts, assembly, runtime, answerRef, judgeRef,
				seqByEvidence, question, latency,
			)
			fmt.Fprintf(os.Stderr, "qa %d/%d %s\n", index+1, len(questions), question.ID)
			mu.Lock()
			scores[index] = score
			mu.Unlock()
			milestoneMu.Lock()
			completed++
			pct := completed * 100 / len(questions)
			fire := false
			if nextMilestone > 0 && nextMilestone <= 100 && pct >= nextMilestone {
				fire = true
				for nextMilestone <= 100 && pct >= nextMilestone {
					nextMilestone += opts.ProgressPct
				}
			}
			milestoneMu.Unlock()
			if fire {
				bestEffortNotify(ctx, n, notifyEvent{
					Kind:  "qa_progress",
					Title: fmt.Sprintf("qa %d/%d", completed, len(questions)),
					Body:  fmt.Sprintf("progress %d%%", pct),
				})
			}
		}(index, question)
	}
	wg.Wait()

	return buildReport(opts, started, scores, latency, assembly.PolicyDigest), nil
}

func evalQuestion(
	ctx context.Context,
	opts runOptions,
	assembly *memoryconfig.Assembly,
	runtime *inference.Runtime,
	answerRef inference.ModelRef,
	judgeRef *inference.ModelRef,
	seqByEvidence map[string]map[string]uint64,
	question dataset.Question,
	latency *latencyAggregator,
) scenarios.QuestionScore {
	score := scenarios.QuestionScore{
		ID:            question.ID,
		Query:         question.Query,
		Tags:          append([]string(nil), question.Tags...),
		EvidenceCount: len(question.EvidenceIDs),
	}
	questionCtx := ctx
	var cancel context.CancelFunc
	if opts.QATimeout > 0 {
		questionCtx, cancel = context.WithTimeout(ctx, opts.QATimeout)
		defer cancel()
	}
	metadata := sdkmemory.Metadata{}
	if question.AskedAt != "" {
		metadata["asked_at"] = question.AskedAt
	}
	recallStart := time.Now()
	result, err := assembly.System.Context(questionCtx, sdkmemory.ContextRequest{
		Scope:          scopeFor(opts.Scenario.RuntimeID(), question.ConversationID),
		ConversationID: question.ConversationID,
		Query:          question.Query,
		Budget:         sdkmemory.Budget{MaxItems: opts.MaxItems, MaxTokens: opts.MaxTokens},
		Metadata:       metadata,
		RecallEventID:  "eval:" + opts.Scenario.Name() + ":" + question.ID,
	})
	latency.record("recall", time.Since(recallStart), err == nil)
	if err != nil {
		score.Error = err.Error()
		return score
	}
	score.ItemCount = len(result.Items)
	if len(question.EvidenceIDs) > 0 {
		hit := computeKHit(result.Items, evidenceSeqs(question.EvidenceIDs, seqByEvidence[question.ConversationID]))
		score.KHit = boolFloatPtr(hit.Hit)
		score.KHitMessage = boolFloatPtr(hit.Message)
		score.KHitFact = boolFloatPtr(hit.Fact)
	}

	content, err := buildAnswerInput(question, result.Items)
	if err != nil {
		score.Error = err.Error()
		return score
	}
	answerStart := time.Now()
	prediction, err := generateWithSystem(questionCtx, runtime, answerRef, answerSystem, content)
	latency.record("answer", time.Since(answerStart), err == nil)
	if err != nil {
		score.Error = err.Error()
		return score
	}
	score.Prediction = prediction
	em, f1, abstention := opts.Scenario.Score(prediction, question, 0, false)
	score.EM, score.F1 = em, f1
	score.Abstention = abstention
	if judgeRef != nil {
		judgeStart := time.Now()
		judge, judgeErr := judgeResponse(questionCtx, runtime, *judgeRef, question.GoldAnswers, prediction)
		latency.record("judge", time.Since(judgeStart), judgeErr == nil)
		if judgeErr != nil {
			score.Error = judgeErr.Error()
			return score
		}
		score.Judge = judge
		score.JudgeScored = true
	}
	return score
}

func boolFloatPtr(value bool) *float64 {
	number := 0.0
	if value {
		number = 1
	}
	return &number
}
