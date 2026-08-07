package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	memoryconfig "github.com/GizClaw/flowcraft/memory/config"
	"github.com/GizClaw/flowcraft/sdk/inference"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
	envresolver "github.com/GizClaw/flowcraft/sdk/inference/config/env"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/inference/azure"
	"github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
	"github.com/GizClaw/flowcraft/sdkx/inference/deepseek"
	"github.com/GizClaw/flowcraft/sdkx/inference/minimax"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
	"github.com/GizClaw/flowcraft/sdkx/inference/qwen"
)

const maxBatchRunes = 12000

type runOptions struct {
	Dataset       *Dataset
	InferencePath string
	Suite         string
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
}

// Run ingests the dataset through a real memory assembly and evaluates every
// question with the sdk-default retrieval profile.
func Run(ctx context.Context, opts runOptions) (*Report, error) {
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
		scope := scopeFor(opts.Suite, conversation.ID)
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
			ctx, assembly, opts.Suite, conversation, seqByEvidence, opts.IngestTimeout,
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
		err := assembly.Runner.ProcessScope(deriveCtx, scopeFor(opts.Suite, conversation.ID))
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
			retryErr := assembly.Runner.ProcessScope(retryCtx, scopeFor(opts.Suite, conversation.ID))
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

	questions := opts.Dataset.Questions
	if opts.Limit > 0 && len(questions) > opts.Limit {
		questions = questions[:opts.Limit]
	}
	scores := make([]questionScore, len(questions))
	latency := newLatencyAggregator()
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, opts.Concurrency)
	)
	for index, question := range questions {
		wg.Add(1)
		go func(index int, question Question) {
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
		}(index, question)
	}
	wg.Wait()

	return buildReport(opts, started, scores, latency, assembly.PolicyDigest), nil
}

func ingestConversation(
	ctx context.Context,
	assembly *memoryconfig.Assembly,
	suite string,
	conversation Conversation,
	seqByEvidence map[string]map[string]uint64,
	timeout time.Duration,
) error {
	ingestCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		ingestCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	scope := scopeFor(suite, conversation.ID)
	batches := batchTurns(conversation.Turns)
	seq := uint64(0)
	for batchIndex, batch := range batches {
		messages := make([]message.Message, 0, len(batch))
		for _, turn := range batch {
			seq++
			if turn.EvidenceID != "" {
				if seqByEvidence[conversation.ID] == nil {
					seqByEvidence[conversation.ID] = make(map[string]uint64)
				}
				seqByEvidence[conversation.ID][turn.EvidenceID] = seq
			}
			messages = append(messages, message.NewTextMessage(turnRole(turn.Role), turn.Content))
		}
		if len(messages) == 0 {
			continue
		}
		if err := assembly.System.CommitTurn(ingestCtx, sdkmemory.Turn{
			Scope:          scope,
			ConversationID: conversation.ID,
			IdempotencyKey: fmt.Sprintf("%s/%d", conversation.ID, batchIndex),
			Messages:       messages,
		}); err != nil {
			return fmt.Errorf("commit conversation %s batch %d: %w", conversation.ID, batchIndex, err)
		}
	}
	return nil
}

func batchTurns(turns []Turn) [][]Turn {
	var (
		batches        [][]Turn
		current        []Turn
		currentSession string
		runes          int
	)
	for _, turn := range turns {
		content := strings.TrimSpace(turn.Content)
		if content == "" {
			continue
		}
		turn.Content = content
		if turn.SessionID != "" && turn.SessionID != currentSession && len(current) > 0 {
			batches = append(batches, current)
			current, currentSession, runes = nil, turn.SessionID, 0
		}
		nextRunes := utf8.RuneCountInString(content)
		if len(current) > 0 && runes+nextRunes > maxBatchRunes {
			batches = append(batches, current)
			current, runes = nil, 0
		}
		current = append(current, turn)
		runes += nextRunes
		if turn.SessionID != "" {
			currentSession = turn.SessionID
		}
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func evalQuestion(
	ctx context.Context,
	opts runOptions,
	assembly *memoryconfig.Assembly,
	runtime *inference.Runtime,
	answerRef inference.ModelRef,
	judgeRef *inference.ModelRef,
	seqByEvidence map[string]map[string]uint64,
	question Question,
	latency *latencyAggregator,
) questionScore {
	score := questionScore{
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
		Scope:          scopeFor(opts.Suite, question.ConversationID),
		ConversationID: question.ConversationID,
		Query:          question.Query,
		Budget:         sdkmemory.Budget{MaxItems: opts.MaxItems, MaxTokens: opts.MaxTokens},
		Metadata:       metadata,
		RecallEventID:  "eval:" + opts.Suite + ":" + question.ID,
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

	prompt := answerPrompt(question, result.Items)
	answerStart := time.Now()
	prediction, err := generateText(questionCtx, runtime, answerRef, prompt)
	latency.record("answer", time.Since(answerStart), err == nil)
	if err != nil {
		score.Error = err.Error()
		return score
	}
	score.Prediction = prediction
	score.EM, score.F1 = scoreEMF1(prediction, question.GoldAnswers)
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

func answerPrompt(question Question, items []sdkmemory.ContextItem) string {
	var prompt strings.Builder
	prompt.WriteString("Answer the question using only the memories below. ")
	prompt.WriteString("If the memories do not contain the answer, answer \"I don't know\".\n")
	if question.AskedAt != "" {
		fmt.Fprintf(&prompt, "The question was asked at: %s\n", question.AskedAt)
	}
	fmt.Fprintf(&prompt, "Question: %s\n\nMemories:\n", question.Query)
	for index, item := range items {
		fmt.Fprintf(&prompt, "%d. [%s/%s] %s\n", index+1, item.SourceClass, item.Kind, item.Content.Text())
	}
	return prompt.String()
}

func generateText(
	ctx context.Context,
	runtime *inference.Runtime,
	model inference.ModelRef,
	prompt string,
) (string, error) {
	response, err := runtime.Generate(ctx, model, inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{message.TextPart{Text: prompt}}},
				Intent:  inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(response.Message.Content.Text()), nil
}

func buildInferenceRuntime(ctx context.Context, path string) (*inference.Runtime, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read inference config: %w", err)
	}
	document, err := inferenceconfig.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse inference config: %w", err)
	}
	builder, err := inferenceconfig.NewBuilder(
		providerFactories(),
		map[string]inferenceconfig.SecretResolver{"env": envresolver.New()},
	)
	if err != nil {
		return nil, fmt.Errorf("inference builder: %w", err)
	}
	runtime, err := builder.NewRuntime(ctx, document)
	if err != nil {
		return nil, fmt.Errorf("build inference runtime: %w", err)
	}
	return runtime, nil
}

func providerFactories() map[string]inferenceconfig.Factory {
	return map[string]inferenceconfig.Factory{
		"openai":    openai.Factory(),
		"azure":     azure.Factory(),
		"deepseek":  deepseek.Factory(),
		"qwen":      qwen.Factory(),
		"bytedance": bytedance.Factory(),
		"minimax":   minimax.Factory(),
	}
}

func parseModelRef(spec string) (inference.ModelRef, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return inference.ModelRef{}, fmt.Errorf("model spec %q must be provider:model[:profile]", spec)
	}
	ref := inference.ModelRef{
		ID: inference.ModelID{Provider: parts[0], Name: parts[1]},
	}
	if len(parts) == 3 {
		ref.Profile = parts[2]
	}
	if err := ref.Validate(); err != nil {
		return inference.ModelRef{}, err
	}
	return ref, nil
}

func toModelSettings(ref inference.ModelRef) memoryconfig.ModelSettings {
	return memoryconfig.ModelSettings{
		Provider: ref.ID.Provider,
		Name:     ref.ID.Name,
		Profile:  ref.Profile,
	}
}

func scopeFor(suite, conversationID string) sdkmemory.Scope {
	return sdkmemory.Scope{RuntimeID: suiteRuntimeID(suite), UserID: conversationID}
}

func suiteRuntimeID(suite string) string {
	if suite == "longmemeval" {
		return "eval-lme"
	}
	return "eval-locomo"
}

func turnRole(role string) message.Role {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return message.RoleAssistant
	case "system":
		return message.RoleSystem
	default:
		return message.RoleUser
	}
}

func boolFloatPtr(value bool) *float64 {
	number := 0.0
	if value {
		number = 1
	}
	return &number
}

type latencyAggregator struct {
	mu     sync.Mutex
	totals map[string]time.Duration
	calls  map[string]int
}

func newLatencyAggregator() *latencyAggregator {
	return &latencyAggregator{
		totals: make(map[string]time.Duration),
		calls:  make(map[string]int),
	}
}

func (aggregator *latencyAggregator) record(stage string, duration time.Duration, ok bool) {
	if !ok {
		return
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	aggregator.totals[stage] += duration
	aggregator.calls[stage]++
}

func (aggregator *latencyAggregator) snapshot() map[string]latencySummary {
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	result := make(map[string]latencySummary, len(aggregator.totals))
	for stage, total := range aggregator.totals {
		result[stage] = latencySummary{
			Calls:   aggregator.calls[stage],
			TotalMs: total.Milliseconds(),
		}
	}
	return result
}
