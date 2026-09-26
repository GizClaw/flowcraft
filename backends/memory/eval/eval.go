// Package eval runs memory quality scenarios against a core/memory assembly.
//
// The harness is dataset-agnostic: a scenario is an ordered list of turns
// followed by questions with expected content. LoCoMo/LongMemEval exports can
// be converted into scenarios by host-side loaders; the metrics here stay the
// same.
package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// Runner is the narrow assembly surface the harness drives.
type Runner interface {
	CommitTurn(context.Context, corememory.Turn) error
	Context(context.Context, corememory.ContextRequest) (corememory.ContextResult, error)
	RunOnce(context.Context) error
}

// Turn is one committed conversation batch.
type Turn struct {
	IdempotencyKey string                `json:"idempotency_key"`
	Messages       []coremessage.Message `json:"messages"`
	// DatasetIDs optionally names the dataset turn each message came from
	// (LoCoMo's dia_id), positionally aligned with Messages. It is ingest-side
	// metadata used only to resolve evidence recall at grading time; it is
	// never sent to retrieval, so it cannot become an oracle channel.
	DatasetIDs []string `json:"dataset_ids,omitempty"`
}

// Question asks for content that the memory should return.
type Question struct {
	Query        string   `json:"query"`
	WantContains []string `json:"want_contains"`
	// Evidence lists the dataset turns that carry the answer (LoCoMo dia_ids).
	// It grades recall against the dataset's own evidence instead of guessing
	// from answer strings. Like DatasetIDs it is never forwarded to retrieval.
	Evidence []string `json:"evidence,omitempty"`
	// AskedAt is the question's own timestamp when the dataset carries one.
	// Relative-time questions ("last year") need it to resolve.
	AskedAt string `json:"asked_at,omitempty"`
	// Category is an optional dataset label (LoCoMo categories, for example)
	// carried through to the report so hit rates can be sliced per category.
	Category int `json:"category,omitempty"`
}

// Scenario is one scripted conversation plus its questions.
type Scenario struct {
	Name           string            `json:"name"`
	Scope          Scope             `json:"scope"`
	ConversationID string            `json:"conversation_id"`
	Budget         corememory.Budget `json:"budget,omitempty"`
	Turns          []Turn            `json:"turns"`
	Questions      []Question        `json:"questions"`
}

// Scope is the JSON-facing tenant scope used by scenarios. It mirrors the
// memory scope document because core/memory types carry no wire tags.
type Scope struct {
	RuntimeID string `json:"runtime_id"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

func (scope Scope) memoryScope() corememory.Scope {
	return corememory.Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID, AgentID: scope.AgentID}
}

// QuestionResult records one graded question.
type QuestionResult struct {
	Query    string `json:"query"`
	Category int    `json:"category,omitempty"`
	// Hit reports full evidence coverage: every dataset turn that carries the
	// answer was surfaced. Questions without dataset evidence fall back to
	// expectation containment.
	Hit bool `json:"hit"`
	// EvidenceTotal and EvidenceFound are the dataset turns expected and
	// surfaced for this question.
	EvidenceTotal int `json:"evidence_total,omitempty"`
	EvidenceFound int `json:"evidence_found,omitempty"`
	// EvidenceRaw counts the evidence turns whose text reached the prompt
	// itself; EvidenceProvenance counts the ones reached only through an item's
	// provenance (a fact that points at the turn). The second kind means the
	// model saw a paraphrase, not the source wording, so the two are reported
	// apart: only the raw kind guarantees the answer's surface form was visible.
	EvidenceRaw        int `json:"evidence_raw,omitempty"`
	EvidenceProvenance int `json:"evidence_provenance,omitempty"`
	// Ungraded marks a question without any non-empty expectation: it has
	// nothing to match, so it is excluded from the hit rate.
	Ungraded bool          `json:"ungraded,omitempty"`
	Items    int           `json:"items"`
	Missing  []string      `json:"missing,omitempty"`
	Latency  time.Duration `json:"latency"`
	// Answer fields are filled when the run has an Answerer: the model answer
	// generated from the recalled context, whether it was graded, and whether
	// it was accepted by the Judge (or by containment).
	Answer       string `json:"answer,omitempty"`
	AnswerGraded bool   `json:"answer_graded,omitempty"`
	AnswerHit    bool   `json:"answer_hit,omitempty"`
	// AnswerUngraded marks an answer the judge could not grade (an unusable
	// verdict, for example). Ungraded answers leave the denominator instead of
	// being counted as hits, and AnswerNote carries the reason.
	AnswerUngraded bool   `json:"answer_ungraded,omitempty"`
	AnswerNote     string `json:"answer_note,omitempty"`
	// Lenient fields carry the optional second (leaderboard-aligned) judge.
	LenientGraded   bool `json:"lenient_graded,omitempty"`
	LenientHit      bool `json:"lenient_hit,omitempty"`
	LenientUngraded bool `json:"lenient_ungraded,omitempty"`
}

// CategoryStats aggregates the graded questions of one dataset category.
type CategoryStats struct {
	Questions int `json:"questions"`
	Hits      int `json:"hits"`
	// Answered and AnswerHits aggregate the generative answering stage.
	Answered   int `json:"answered,omitempty"`
	AnswerHits int `json:"answer_hits,omitempty"`
	// LenientAnswered and LenientAnswerHits aggregate the second judge.
	LenientAnswered   int `json:"lenient_answered,omitempty"`
	LenientAnswerHits int `json:"lenient_answer_hits,omitempty"`
}

// Report aggregates one scenario run.
type Report struct {
	Scenario    string                `json:"scenario"`
	Questions   []QuestionResult      `json:"questions"`
	Hits        int                   `json:"hits"`
	Ungraded    int                   `json:"ungraded,omitempty"`
	ByCategory  map[int]CategoryStats `json:"by_category,omitempty"`
	HitRate     float64               `json:"hit_rate"`
	MeanLatency time.Duration         `json:"mean_latency"`
	// EvidenceTotal and EvidenceFound aggregate the dataset turns expected and
	// surfaced across the scenario, and EvidenceRecall is their ratio: the
	// turn-level recall the LoCoMo protocol reports.
	EvidenceTotal      int     `json:"evidence_total,omitempty"`
	EvidenceFound      int     `json:"evidence_found,omitempty"`
	EvidenceRecall     float64 `json:"evidence_recall,omitempty"`
	EvidenceRaw        int     `json:"evidence_raw,omitempty"`
	EvidenceProvenance int     `json:"evidence_provenance,omitempty"`
	// Answers, AnswerHits, and AnswerRate are filled when a run is configured
	// with an Answerer: they grade a model answer generated from the recalled
	// context instead of grading the context itself.
	Answers    int     `json:"answers,omitempty"`
	AnswerHits int     `json:"answer_hits,omitempty"`
	AnswerRate float64 `json:"answer_rate,omitempty"`
	// UngradedAnswers counts answers the judge could not grade; they are
	// excluded from AnswerRate and reported so a flaky judge is never silent.
	UngradedAnswers int `json:"ungraded_answers,omitempty"`
	// LenientAnswers, LenientHits, and LenientRate carry the optional second
	// (leaderboard-aligned) judge, so both protocols are reported.
	LenientAnswers  int     `json:"lenient_answers,omitempty"`
	LenientHits     int     `json:"lenient_answer_hits,omitempty"`
	LenientRate     float64 `json:"lenient_answer_rate,omitempty"`
	LenientUngraded int     `json:"lenient_ungraded,omitempty"`
}

// Run commits every turn, runs one derivation pass, and grades the questions.
func Run(ctx context.Context, runner Runner, scenario Scenario) (Report, error) {
	return RunWithOptions(ctx, runner, scenario, Options{})
}

// RunWithOptions is Run plus the optional generative answering stage: each
// question is answered from the recalled context and the answer is graded by
// the Judge (or by containment when no judge is configured). The recall
// fields stay populated, so both metrics are reported side by side.
func RunWithOptions(ctx context.Context, runner Runner, scenario Scenario, options Options) (Report, error) {
	if err := Ingest(ctx, runner, scenario); err != nil {
		return Report{}, err
	}
	if err := Derive(ctx, runner); err != nil {
		return Report{}, err
	}
	return Answer(ctx, runner, scenario, options)
}

// Ingest commits every turn of one scenario without deriving or answering.
// Hosts that measure several conversations can ingest them all first and then
// call Derive once, so the worker fans out across every conversation that has
// pending commits instead of paying for one serial pass per scenario.
func Ingest(ctx context.Context, runner Runner, scenario Scenario) error {
	if ctx == nil {
		return errors.New("memory eval: context is required")
	}
	if runner == nil {
		return errors.New("memory eval: runner is required")
	}
	scope := scenario.Scope.memoryScope()
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("memory eval: %w", err)
	}
	if strings.TrimSpace(scenario.ConversationID) == "" {
		return errors.New("memory eval: conversation_id is required")
	}
	for index, turn := range scenario.Turns {
		if strings.TrimSpace(turn.IdempotencyKey) == "" || len(turn.Messages) == 0 {
			return fmt.Errorf("memory eval: turn %d requires an idempotency key and messages", index)
		}
		if len(turn.DatasetIDs) > 0 && len(turn.DatasetIDs) != len(turn.Messages) {
			return fmt.Errorf("memory eval: turn %d has %d dataset ids for %d messages",
				index, len(turn.DatasetIDs), len(turn.Messages))
		}
		if err := runner.CommitTurn(ctx, corememory.Turn{
			Scope: scope, ConversationID: scenario.ConversationID,
			IdempotencyKey: turn.IdempotencyKey, Messages: turn.Messages,
		}); err != nil {
			return fmt.Errorf("memory eval: commit turn %d: %w", index, err)
		}
	}
	return nil
}

// Derive runs one derivation pass over everything ingested so far. It is
// resumable by design: a conversation that fails keeps its own watermark, so a
// retry picks it up where it stopped.
func Derive(ctx context.Context, runner Runner) error {
	if ctx == nil {
		return errors.New("memory eval: context is required")
	}
	if runner == nil {
		return errors.New("memory eval: runner is required")
	}
	if err := runner.RunOnce(ctx); err != nil {
		return fmt.Errorf("memory eval: derive: %w", err)
	}
	return nil
}

// Answer grades one already-ingested scenario: every question is retrieved,
// graded for evidence recall, and (when configured) answered and judged.
func Answer(ctx context.Context, runner Runner, scenario Scenario, options Options) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("memory eval: context is required")
	}
	if runner == nil {
		return Report{}, errors.New("memory eval: runner is required")
	}
	if options.Answerer == nil && options.Judge != nil {
		return Report{}, errors.New("memory eval: a judge requires an answerer")
	}
	if options.Answerer == nil && options.LenientJudge != nil {
		return Report{}, errors.New("memory eval: a lenient judge requires an answerer")
	}
	scope := scenario.Scope.memoryScope()
	if err := scope.Validate(); err != nil {
		return Report{}, fmt.Errorf("memory eval: %w", err)
	}
	if strings.TrimSpace(scenario.ConversationID) == "" {
		return Report{}, errors.New("memory eval: conversation_id is required")
	}
	report := Report{Scenario: scenario.Name}
	var total time.Duration
	graded := 0
	categories := make(map[int]CategoryStats)
	evidence := newEvidenceIndex(scenario.Turns)
	outcomes := runQuestions(ctx, runner, scenario, evidence, options)
	for _, outcome := range outcomes {
		if outcome.err != nil {
			return Report{}, outcome.err
		}
		grade := outcome.grade
		total += grade.Latency
		report.EvidenceTotal += grade.EvidenceTotal
		report.EvidenceFound += grade.EvidenceFound
		report.EvidenceRaw += grade.EvidenceRaw
		report.EvidenceProvenance += grade.EvidenceProvenance
		if grade.Ungraded {
			report.Questions = append(report.Questions, grade)
			report.Ungraded++
			continue
		}
		if options.Answerer != nil {
			report.Answers++
			switch {
			case grade.AnswerUngraded:
				report.UngradedAnswers++
			case grade.AnswerHit:
				report.AnswerHits++
			}
			if grade.LenientGraded || grade.LenientUngraded {
				if !grade.LenientUngraded {
					report.LenientAnswers++
				} else {
					report.LenientUngraded++
				}
				if grade.LenientHit {
					report.LenientHits++
				}
			}
			if grade.Category != 0 {
				stats := categories[grade.Category]
				if grade.AnswerGraded {
					stats.Answered++
					if grade.AnswerHit {
						stats.AnswerHits++
					}
				}
				if grade.LenientGraded {
					stats.LenientAnswered++
					if grade.LenientHit {
						stats.LenientAnswerHits++
					}
				}
				categories[grade.Category] = stats
			}
		}
		report.Questions = append(report.Questions, grade)
		graded++
		if grade.Hit {
			report.Hits++
		}
		if grade.Category != 0 {
			stats := categories[grade.Category]
			stats.Questions++
			if grade.Hit {
				stats.Hits++
			}
			categories[grade.Category] = stats
		}
	}
	if graded > 0 {
		report.HitRate = float64(report.Hits) / float64(graded)
	}
	if report.Answers > 0 {
		gradedAnswers := report.Answers - report.UngradedAnswers
		if gradedAnswers > 0 {
			report.AnswerRate = float64(report.AnswerHits) / float64(gradedAnswers)
		}
	}
	if report.LenientAnswers > 0 {
		report.LenientRate = float64(report.LenientHits) / float64(report.LenientAnswers)
	}
	if report.EvidenceTotal > 0 {
		report.EvidenceRecall = float64(report.EvidenceFound) / float64(report.EvidenceTotal)
	}
	if len(report.Questions) > 0 {
		report.MeanLatency = total / time.Duration(len(report.Questions))
	}
	if len(categories) > 0 {
		report.ByCategory = categories
	}
	return report, nil
}

// RunAll runs scenarios in order; the first failure stops the batch.
func RunAll(ctx context.Context, runner Runner, scenarios []Scenario) ([]Report, error) {
	return RunAllWithOptions(ctx, runner, scenarios, Options{})
}

// questionOutcome is one question's finished work: the graded result, or the
// error that aborted it.
type questionOutcome struct {
	grade QuestionResult
	err   error
}

// runQuestions drives the per-question pipeline (retrieve, grade evidence,
// answer, judge) and returns the outcomes in question order.
//
// Concurrency (Options.Concurrency) only changes the schedule, never the
// protocol: every question keeps its own request and prompt, retrieval stays
// read-only once derivation is done, and outcomes are written back by index, so
// a report is identical to the sequential one for a deterministic model.
func runQuestions(ctx context.Context, runner Runner, scenario Scenario, evidence evidenceIndex, options Options) []questionOutcome {
	questions := scenario.Questions
	outcomes := make([]questionOutcome, len(questions))
	work := func(index int) {
		question := questions[index]
		started := time.Now()
		result, err := runner.Context(ctx, corememory.ContextRequest{
			Scope: scenario.Scope.memoryScope(), ConversationID: scenario.ConversationID,
			// Dataset evidence ids deliberately stay out of the request: they
			// grade the result, they must not steer retrieval.
			Query: question.Query, Budget: scenario.Budget,
		})
		if err != nil {
			outcomes[index] = questionOutcome{err: fmt.Errorf("memory eval: question %q: %w", question.Query, err)}
			return
		}
		latency := time.Since(started)
		grade := gradeQuestion(ctx, question, result.Items, evidence, options.Provenance)
		grade.Latency = latency
		grade.Items = len(result.Items)
		outcome := questionOutcome{grade: grade}
		if grade.Ungraded {
			outcomes[index] = outcome
			return
		}
		if options.Answerer != nil {
			answer, answerErr := options.Answerer.Answer(ctx, question, result.Items)
			if answerErr != nil {
				outcomes[index] = questionOutcome{
					err: fmt.Errorf("memory eval: answer question %q: %w", question.Query, answerErr),
				}
				return
			}
			outcome.grade.Answer = answer
			if options.Judge != nil {
				correct, judgeErr := options.Judge.Judge(ctx, question, answer)
				if judgeErr != nil {
					outcome.grade.AnswerUngraded = true
					outcome.grade.AnswerNote = judgeErr.Error()
				} else {
					outcome.grade.AnswerGraded = true
					outcome.grade.AnswerHit = correct
				}
			} else {
				outcome.grade.AnswerGraded = true
				outcome.grade.AnswerHit = containsAll(answer, question.WantContains)
			}
			if options.LenientJudge != nil {
				lenient, lenientErr := options.LenientJudge.Judge(ctx, question, answer)
				if lenientErr != nil {
					outcome.grade.LenientUngraded = true
					if outcome.grade.AnswerNote == "" {
						outcome.grade.AnswerNote = lenientErr.Error()
					}
				} else {
					outcome.grade.LenientGraded = true
					outcome.grade.LenientHit = lenient
				}
			}
		}
		outcomes[index] = outcome
	}

	concurrency := options.Concurrency
	if concurrency > len(questions) {
		concurrency = len(questions)
	}
	if concurrency <= 1 {
		for index := range questions {
			work(index)
		}
		return outcomes
	}
	// Fan out over the question indexes: workers take the next index and write
	// only their own slot, so no result is shared or reordered.
	var (
		next  atomic.Int64
		group sync.WaitGroup
	)
	for worker := 0; worker < concurrency; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(questions) {
					return
				}
				work(index)
			}
		}()
	}
	group.Wait()
	return outcomes
}

// RunAllWithOptions runs scenarios in order with the optional answering
// stage; the first failure stops the batch.
func RunAllWithOptions(ctx context.Context, runner Runner, scenarios []Scenario, options Options) ([]Report, error) {
	reports := make([]Report, 0, len(scenarios))
	for _, scenario := range scenarios {
		report, err := RunWithOptions(ctx, runner, scenario, options)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// evidenceIndex maps a dataset turn id (LoCoMo dia_id) to the exact text the
// harness committed for it, so a recalled item can be matched back to the turn
// that carries the answer.
type evidenceIndex map[string]string

func newEvidenceIndex(turns []Turn) evidenceIndex {
	index := make(evidenceIndex, len(turns))
	for _, turn := range turns {
		for position, message := range turn.Messages {
			if position >= len(turn.DatasetIDs) {
				break
			}
			id := strings.TrimSpace(turn.DatasetIDs[position])
			text := strings.TrimSpace(message.Content.Text())
			if id == "" || text == "" {
				continue
			}
			if _, exists := index[id]; !exists {
				index[id] = text
			}
		}
	}
	return index
}

// gradeQuestion scores evidence recall when the dataset names the turns that
// carry the answer, and falls back to expectation containment otherwise.
func gradeQuestion(ctx context.Context, question Question, items []corememory.ContextItem, index evidenceIndex, resolver ProvenanceResolver) QuestionResult {
	result := QuestionResult{Query: question.Query, Category: question.Category}
	if evidence := uniqueNonEmpty(question.Evidence); len(evidence) > 0 {
		result.EvidenceTotal = len(evidence)
		resolved := make(map[string][]string, len(items))
		for _, id := range evidence {
			turnText := index[id]
			if turnText == "" {
				result.Missing = append(result.Missing, id)
				continue
			}
			hit, viaRaw := recalledTurn(ctx, items, turnText, resolver, resolved)
			if !hit {
				result.Missing = append(result.Missing, id)
				continue
			}
			result.EvidenceFound++
			if viaRaw {
				result.EvidenceRaw++
			} else {
				result.EvidenceProvenance++
			}
		}
		result.Hit = result.EvidenceFound == result.EvidenceTotal
		return result
	}
	wants := make([]string, 0, len(question.WantContains))
	for _, want := range question.WantContains {
		if trimmed := strings.TrimSpace(want); trimmed != "" {
			wants = append(wants, trimmed)
		}
	}
	if len(wants) == 0 {
		// Nothing to match: the question cannot be graded, and counting it
		// as an empty hit or a guaranteed miss would skew the hit rate.
		result.Ungraded = true
		return result
	}
	var corpus strings.Builder
	for _, item := range items {
		corpus.WriteString(item.Content.Text())
		corpus.WriteByte('\n')
	}
	text := corpus.String()
	hit := true
	for _, want := range wants {
		if !containsFold(text, want) {
			hit = false
			result.Missing = append(result.Missing, want)
		}
	}
	result.Hit = hit
	return result
}

// recalledTurn reports whether any recalled item covers the dataset turn whose
// committed text is turnText: the item either carries the text itself (raw
// message items) or resolves to it through provenance (facts point at the
// messages they were derived from). resolved memoizes source texts per item,
// so one question never resolves the same item twice.
func recalledTurn(ctx context.Context, items []corememory.ContextItem, turnText string, resolver ProvenanceResolver, resolved map[string][]string) (found, viaRaw bool) {
	for _, item := range items {
		if strings.Contains(item.Content.Text(), turnText) {
			return true, true
		}
		if resolver == nil {
			continue
		}
		texts, cached := resolved[item.ID]
		if !cached {
			texts = resolver.ResolveSourceTexts(ctx, item)
			resolved[item.ID] = texts
		}
		for _, text := range texts {
			if strings.Contains(text, turnText) {
				return true, false
			}
		}
	}
	return false, false
}

// containsFold is the containment rule shared with the answer fallback grader:
// case-insensitive, so recall and answers cannot disagree about the same
// expectation. The old case-sensitive comparison systematically understated
// recall for datasets that rely on expectation containment (LongMemEval).
func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(strings.TrimSpace(needle)))
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique
}

// LoadScenario decodes one strict JSON scenario document.
func LoadScenario(reader io.Reader) (Scenario, error) {
	var scenario Scenario
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&scenario); err != nil {
		return Scenario{}, fmt.Errorf("memory eval: decode scenario: %w", err)
	}
	return scenario, nil
}

// LoadScenariosJSONL decodes one scenario per line, skipping blank lines and
// "#" comments so dataset exports can carry provenance headers.
func LoadScenariosJSONL(reader io.Reader) ([]Scenario, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var scenarios []Scenario
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		scenario, err := LoadScenario(strings.NewReader(text))
		if err != nil {
			return nil, fmt.Errorf("memory eval: line %d: %w", line, err)
		}
		scenarios = append(scenarios, scenario)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("memory eval: read scenarios: %w", err)
	}
	return scenarios, nil
}
