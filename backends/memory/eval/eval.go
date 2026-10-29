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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// Runner is the narrow assembly surface the harness drives.
type Runner interface {
	CommitTurn(context.Context, corememory.Turn) error
	Context(context.Context, corememory.ContextRequest) (corememory.ContextResult, error)
	RunOnce(context.Context) error
}

// Maintainer is the maintenance pass the harness drives when a run measures
// what a pass wrote. It is a separate interface rather than a fourth method on
// Runner because it is a separate schedule: a run that measures the read path
// of what derivation wrote does not maintain, and the profile block is exactly
// the case that does -- an empty block is served until a pass fills it, which a
// report that never maintained would measure as if the block were the feature.
type Maintainer interface {
	Maintain(context.Context, corememory.Scope) (maintain.Plan, error)
}

// DatasetTurnMetadataKey is the per-message metadata key ingest writes the
// dataset turn id under (LoCoMo's dia_id). It lands on the canonical message
// record, never on a retrieval request, and it is what lets a recalled item be
// matched to its dataset turn by identity instead of by committed text: the
// loader's rendering of an image turn and the text the store holds for it
// differ (a caption in the text, or an attached part), and a run whose images
// were fetched only during one of the two passes renders differently.
const DatasetTurnMetadataKey = "dataset_turn_id"

// Turn is one committed conversation batch.
type Turn struct {
	IdempotencyKey string                `json:"idempotency_key"`
	Messages       []coremessage.Message `json:"messages"`
	// DatasetIDs optionally names the dataset turn each message came from
	// (LoCoMo's dia_id), positionally aligned with Messages. Ingest keeps it on
	// the committed message under DatasetTurnMetadataKey, and grading uses it to
	// resolve evidence recall; it is never sent to retrieval, so it cannot
	// become an oracle channel.
	DatasetIDs []string `json:"dataset_ids,omitempty"`
	// ObservedAt optionally carries each message observation anchor
	// (RFC3339 UTC, positionally aligned with Messages), written into
	// the committed message under the core memory observed_at key. An
	// observed-at conversion fills it with the session date time; a
	// session whose date is unknown or unparsable leaves the entries
	// empty. Like DatasetIDs it is never sent to retrieval, so it
	// cannot become an oracle channel.
	ObservedAt []string `json:"observed_at,omitempty"`
}

// Question asks for content that the memory should return.
type Question struct {
	Query        string   `json:"query"`
	WantContains []string `json:"want_contains"`
	// Evidence lists the dataset turns that carry the answer (LoCoMo dia_ids).
	// It grades recall against the dataset's own evidence instead of guessing
	// from answer strings. Like DatasetIDs it is never forwarded to retrieval.
	Evidence []string `json:"evidence,omitempty"`
	// EvidenceSessions lists the dataset sessions that carry the answer, for
	// datasets that label their gold at session granularity (LongMemEval). A
	// session counts as recalled when any message of it was surfaced, which is
	// the recall definition the dataset's own papers report; grading it as
	// "every message of the session" instead would make a hit a function of how
	// long the gold session happens to be. Evidence wins when a question carries
	// both, because a turn is the narrower statement of what the answer needs.
	// Never forwarded to retrieval.
	EvidenceSessions []string `json:"evidence_sessions,omitempty"`
	// AskedAt is the question's own timestamp when the dataset carries one.
	// Relative-time questions ("last year") need it to resolve.
	AskedAt string `json:"asked_at,omitempty"`
	// Category is an optional dataset label (LoCoMo categories, for example)
	// carried through to the report so hit rates can be sliced per category.
	Category int `json:"category,omitempty"`
	// CategoryLabel names Category in the dataset's own words ("multi-session",
	// "temporal-reasoning"). Two datasets number their categories for
	// themselves -- LongMemEval's ids start at 101 so they cannot be read as
	// LoCoMo's -- so a report states the label next to the bucket.
	CategoryLabel string `json:"category_label,omitempty"`
	// Unanswerable marks a question the dataset itself declares unanswerable:
	// its history does not hold the fact, so the graded behaviour is saying so
	// rather than producing an answer. The judge grades it on that, and the
	// answering stage keeps it out of retrieval either way.
	Unanswerable bool `json:"unanswerable,omitempty"`
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

// MemoryScope is the hard partition this scenario's scope names, for a host
// that drives the assembly outside the harness -- running the maintenance pass
// a run measures, for one.
func (scope Scope) MemoryScope() corememory.Scope { return scope.memoryScope() }

// QuestionResult records one graded question.
type QuestionResult struct {
	Query         string `json:"query"`
	Category      int    `json:"category,omitempty"`
	CategoryLabel string `json:"category_label,omitempty"`
	// Unanswerable marks a question the dataset declares unanswerable, so a
	// stored report says which rows were graded on a refusal.
	Unanswerable bool `json:"unanswerable,omitempty"`
	// Hit reports full evidence coverage: every dataset turn that carries the
	// answer was surfaced. Questions without dataset evidence fall back to
	// expectation containment.
	Hit bool `json:"hit"`
	// SessionTotal and SessionFound are the dataset evidence sessions expected
	// and surfaced, for a dataset that labels its gold by session. They are a
	// separate unit from the turn counters below and are kept apart so the two
	// recall figures are never added together.
	SessionTotal int `json:"session_total,omitempty"`
	SessionFound int `json:"session_found,omitempty"`
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
	// Label names the category in the dataset's own words. It is filled from
	// the graded questions, so a report carries the meaning of the bucket it
	// prints next to the number.
	Label     string `json:"label,omitempty"`
	Questions int    `json:"questions"`
	Hits      int    `json:"hits"`
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
	// SessionTotal and SessionFound aggregate session-level evidence (datasets
	// that name gold sessions instead of gold turns), and SessionRecall is
	// their ratio.
	SessionTotal  int     `json:"session_total,omitempty"`
	SessionFound  int     `json:"session_found,omitempty"`
	SessionRecall float64 `json:"session_recall,omitempty"`
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
		if len(turn.ObservedAt) > 0 && len(turn.ObservedAt) != len(turn.Messages) {
			return fmt.Errorf("memory eval: turn %d has %d observed_at entries for %d messages",
				index, len(turn.ObservedAt), len(turn.Messages))
		}
		if err := runner.CommitTurn(ctx, corememory.Turn{
			Scope: scope, ConversationID: scenario.ConversationID,
			IdempotencyKey: turn.IdempotencyKey, Messages: turn.Messages,
			MessageMetadata: messageMetadata(turn),
		}); err != nil {
			return fmt.Errorf("memory eval: commit turn %d: %w", index, err)
		}
	}
	return nil
}

// messageMetadata merges the per-message tags ingest keeps on the committed
// message: the dataset turn identity the grading side reads back, and the
// observed_at anchor an observed-at conversion wrote. A message the dataset
// does not name -- or whose session carried no anchor -- stays untagged
// rather than carrying an empty tag that would look like a resolved value.
func messageMetadata(turn Turn) []corememory.Metadata {
	if len(turn.DatasetIDs) == 0 && len(turn.ObservedAt) == 0 {
		return nil
	}
	metadata := make([]corememory.Metadata, len(turn.Messages))
	tag := func(index int, key, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if metadata[index] == nil {
			metadata[index] = corememory.Metadata{}
		}
		metadata[index][key] = value
	}
	for index, id := range turn.DatasetIDs {
		tag(index, DatasetTurnMetadataKey, id)
	}
	for index, anchor := range turn.ObservedAt {
		tag(index, corememory.ObservedAtMetadataKey, anchor)
	}
	return metadata
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

// MaintainResult reports what one maintenance pass over a set of scopes did.
type MaintainResult struct {
	// Scopes counts the passes this call ran: one per distinct hard partition.
	Scopes int `json:"scopes"`
	// Supersedes counts the findings the passes made: the facts they judged no
	// longer current, by either of their two rules or by the arbiter. It is the
	// denominator ArbiterSupersedes is read against.
	Supersedes int `json:"supersedes,omitempty"`
	// ArbiterAsked counts the candidate pairs the passes put to the arbiter and
	// ArbiterSkipped the ones their budget left for a later pass. A run whose
	// skipped count is not zero measured an arbiter that was not offered the
	// scope, which is a cost bound the deployment should raise rather than a
	// reading -- the pairs a pass skips are judged by none.
	ArbiterAsked   int `json:"arbiter_asked,omitempty"`
	ArbiterSkipped int `json:"arbiter_skipped,omitempty"`
	// ArbiterUndecided counts the pairs the arbiter answered below its
	// threshold: asked, and deliberately left current.
	ArbiterUndecided int `json:"arbiter_undecided,omitempty"`
	// ArbiterSupersedes counts the findings the arbiter made, as opposed to the
	// rules': what the deployment paid a model for.
	ArbiterSupersedes int `json:"arbiter_supersedes,omitempty"`
	// ReclaimedProjectionObjects counts the lane storage the passes removed:
	// the bases and segments a compaction or a rebuild superseded. It is what
	// says on a run's own log that the workspace did not grow by its history.
	ReclaimedProjectionObjects int `json:"reclaimed_projection_objects,omitempty"`
	// ReclaimedChunks counts the superseded document chunk records the passes
	// removed.
	ReclaimedChunks int `json:"reclaimed_chunks,omitempty"`
}

// Maintain runs one maintenance pass per scope and reports what the passes did:
// the pass that saves the read-path overlay, fills the resident profile block,
// and reclaims the derived storage no manifest names.
// It is what a run that measures either has to do between deriving and
// answering, and it is deliberately not folded into Derive: a pass costs the
// scope's facts, and a run that does not measure a pass must not pay for one.
//
// A runner that cannot maintain fails here rather than measuring a store no
// pass touched -- the same direction the fingerprint's resume check fails in.
func Maintain(ctx context.Context, runner Runner, scopes []corememory.Scope) (MaintainResult, error) {
	result := MaintainResult{}
	if ctx == nil {
		return result, errors.New("memory eval: context is required")
	}
	if runner == nil {
		return result, errors.New("memory eval: runner is required")
	}
	maintainer, ok := runner.(Maintainer)
	if !ok {
		return result, errors.New("memory eval: the runner does not maintain")
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return result, fmt.Errorf("memory eval: maintain: %w", err)
		}
		if _, dup := seen[scope.HardPartitionKey()]; dup {
			continue
		}
		seen[scope.HardPartitionKey()] = struct{}{}
		plan, err := maintainer.Maintain(ctx, scope)
		if err != nil {
			return result, fmt.Errorf("memory eval: maintain: %w", err)
		}
		result.Scopes++
		result.Supersedes += len(plan.Supersedes)
		for _, finding := range plan.Supersedes {
			if finding.Reason == maintain.ReasonArbiter {
				result.ArbiterSupersedes++
			}
		}
		result.ArbiterAsked += plan.ArbiterAsked
		result.ArbiterSkipped += plan.ArbiterSkipped
		result.ArbiterUndecided += plan.ArbiterUndecided
		result.ReclaimedProjectionObjects += plan.ReclaimedProjectionObjects
		result.ReclaimedChunks += plan.ReclaimedChunks
	}
	return result, nil
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
		report.SessionTotal += grade.SessionTotal
		report.SessionFound += grade.SessionFound
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
				stats.Label = grade.CategoryLabel
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
			stats.Label = grade.CategoryLabel
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
	if report.SessionTotal > 0 {
		report.SessionRecall = float64(report.SessionFound) / float64(report.SessionTotal)
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

// turnsOfSession lists the dataset turn ids that belong to one session. A gold
// session is recalled when any of them was surfaced, so grading needs the
// session grouping even though the index itself is flat: the ids a session
// conversion derives carry it (see LongMemEvalTurnID), and an id that carries
// none of it -- a LoCoMo dia_id -- belongs to no session.
func (index evidenceIndex) turnsOfSession(sessionID string) []string {
	ids := make([]string, 0, 8)
	for id := range index {
		if session, ok := LongMemEvalSessionOfTurn(id); ok && session == sessionID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// gradeSessionEvidence scores recall against gold session ids. A session counts
// as recalled when any message in it was surfaced -- by its own wording, or
// through an item's provenance. Hit keeps its meaning ("every gold item was
// recovered") at the session granularity the dataset labels, and the session
// counters stay apart from the turn counters so the two recall figures are
// never summed into one number.
func gradeSessionEvidence(
	ctx context.Context,
	question Question,
	items []corememory.ContextItem,
	index evidenceIndex,
	resolver ProvenanceResolver,
	result QuestionResult,
	sessions []string,
) QuestionResult {
	matcher := NewMatcher(resolver)
	result.SessionTotal = len(sessions)
	for _, sessionID := range sessions {
		turnIDs := index.turnsOfSession(sessionID)
		if len(turnIDs) == 0 {
			// The conversion rendered no message for this session, so nothing
			// could ever cover it. It stays in the denominator and is named in
			// Missing rather than silently shrinking the question's gold.
			result.Missing = append(result.Missing, sessionID)
			continue
		}
		covered := false
		for _, turnID := range turnIDs {
			if matcher.Cover(ctx, items, turnID, index[turnID]).Covered() {
				covered = true
				break
			}
		}
		if !covered {
			result.Missing = append(result.Missing, sessionID)
			continue
		}
		result.SessionFound++
	}
	result.Hit = result.SessionFound == result.SessionTotal
	return result
}

// gradeQuestion scores evidence recall when the dataset names the turns that
// carry the answer, and falls back to expectation containment otherwise.
func gradeQuestion(ctx context.Context, question Question, items []corememory.ContextItem, index evidenceIndex, resolver ProvenanceResolver) QuestionResult {
	result := QuestionResult{
		Query: question.Query, Category: question.Category,
		CategoryLabel: question.CategoryLabel, Unanswerable: question.Unanswerable,
	}
	turnEvidence := uniqueNonEmpty(question.Evidence)
	// A question that names its evidence at both granularities is graded at the
	// narrower one: a turn is what the answer actually needs, a session is the
	// best a dataset with coarser labels can say.
	if len(turnEvidence) == 0 {
		if sessions := uniqueNonEmpty(question.EvidenceSessions); len(sessions) > 0 {
			return gradeSessionEvidence(ctx, question, items, index, resolver, result, sessions)
		}
	}
	if len(turnEvidence) > 0 {
		evidence := turnEvidence
		result.EvidenceTotal = len(evidence)
		matcher := NewMatcher(resolver)
		for _, id := range evidence {
			turnText := index[id]
			if turnText == "" {
				result.Missing = append(result.Missing, id)
				continue
			}
			coverage := matcher.Cover(ctx, items, id, turnText)
			if !coverage.Covered() {
				result.Missing = append(result.Missing, id)
				continue
			}
			result.EvidenceFound++
			if coverage.ViaRaw {
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
