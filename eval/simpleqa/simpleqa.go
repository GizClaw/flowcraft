// Package simpleqa runs OpenAI's [SimpleQA] short-form factuality
// benchmark (4 326 factual single-turn questions, 2024) against any
// FlowCraft LLM. It is intentionally the simplest eval in this tree:
// a model answers each question and an LLM-as-judge grades the answer
// against the gold target.
//
// Why SimpleQA, given we already ship eval/locomo and friends:
//
//   - Locomo / LongMemEval / history all measure memory recall under
//     a known context. They tell us nothing about a model's
//     factual ceiling.
//   - SimpleQA is calibration-aware: a model that doesn't know is
//     SUPPOSED to abstain. The headline metric is the
//     "correct-given-attempted" ratio (CORRECT / (CORRECT + INCORRECT))
//     which rewards models that say "I don't know" instead of
//     hallucinating.
//   - It composes with everything else: pair the same model on
//     LongMemEval (memory) and SimpleQA (knowledge) and we get a 2x2
//     view that scales with future "agentic" variants (SimpleQA +
//     web-search, SimpleQA + sdk/knowledge-backed RAG).
//
// Roadmap: a follow-up commit will add a knowledge-grounded variant
// that wraps the answer LLM in sdk/agent + sdk/knowledge.Search so we
// can compare "raw model" vs "model + retrieval" calibration.
//
// [SimpleQA]: https://openai.com/index/introducing-simpleqa/
package simpleqa

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// Question is a single SimpleQA row. The original CSV ships with a
// metadata column that JSON-encodes topic + answer_type + source URLs;
// we lift the two that matter into typed fields and keep the raw blob
// on Metadata for callers that want to slice by source / annotator.
type Question struct {
	ID         string            `json:"id"`
	Problem    string            `json:"problem"`
	Answer     string            `json:"answer"`
	Topic      string            `json:"topic,omitempty"`
	AnswerType string            `json:"answer_type,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Dataset is the loaded question set. Name typically derives from the
// source filename so a Report can be sliced by run later.
type Dataset struct {
	Name      string
	Questions []Question
}

// Verdict is one of three buckets OpenAI defines. The exact wording
// is mirrored from the official grading rubric so our reports are
// comparable to the published numbers.
type Verdict string

const (
	VerdictCorrect      Verdict = "correct"
	VerdictIncorrect    Verdict = "incorrect"
	VerdictNotAttempted Verdict = "not_attempted"
)

// QuestionResult is the per-row payload preserved on the report (up
// to Options.MaxSamples) so a debugging session can inspect the worst
// regressions without re-running the eval.
type QuestionResult struct {
	ID        string  `json:"id"`
	Topic     string  `json:"topic,omitempty"`
	Question  string  `json:"question"`
	Gold      string  `json:"gold"`
	Predicted string  `json:"predicted"`
	Verdict   Verdict `json:"verdict"`
}

// TopicReport breaks the headline numbers down by question topic
// (Geography / Science / etc.). Useful when a model regresses on a
// single category but maintains its overall accuracy.
type TopicReport struct {
	N                 int     `json:"n"`
	Correct           int     `json:"correct"`
	Incorrect         int     `json:"incorrect"`
	NotAttempted      int     `json:"not_attempted"`
	Accuracy          float64 `json:"accuracy"`
	AttemptedAccuracy float64 `json:"attempted_accuracy"`
	AbstentionRate    float64 `json:"abstention_rate"`
}

// Report is the top-level JSON document the cmd writes.
type Report struct {
	Dataset    string    `json:"dataset"`
	Model      string    `json:"model"`
	Judge      string    `json:"judge"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`

	N             int `json:"n"`
	Correct       int `json:"correct"`
	Incorrect     int `json:"incorrect"`
	NotAttempted  int `json:"not_attempted"`
	JudgeFailures int `json:"judge_failures"` // judge returned something we couldn't parse

	// Accuracy = Correct / N. Standard.
	Accuracy float64 `json:"accuracy"`
	// AttemptedAccuracy = Correct / (Correct + Incorrect). The
	// calibration metric — a model that abstains rather than
	// hallucinates scores higher here even with a lower raw
	// Accuracy.
	AttemptedAccuracy float64 `json:"attempted_accuracy"`
	// AbstentionRate = NotAttempted / N. Reports how often the
	// model declined to answer.
	AbstentionRate float64 `json:"abstention_rate"`
	// HallucinationRate = Incorrect / N. Mirror of AttemptedAccuracy
	// looked at from the "how often did it answer wrong" angle.
	HallucinationRate float64 `json:"hallucination_rate"`

	PerTopic map[string]*TopicReport `json:"per_topic,omitempty"`
	Samples  []QuestionResult        `json:"samples,omitempty"`

	Options map[string]any `json:"options"`
}

// Event is the canonical lifecycle event shape used by every eval suite.
type Event struct {
	Kind   string
	Time   time.Time
	Title  string
	Body   string
	Fields map[string]string
}

// EventHook receives lifecycle events when set.
type EventHook func(ctx context.Context, e Event)

// DefaultAnswerPrompt is the bare-bones template the official SimpleQA
// eval uses: no system role, just the question. Models are free to
// preface their reply with hedging language; the judge handles that.
const DefaultAnswerPrompt = "{question}"

// GradePrompt is OpenAI's official SimpleQA judge prompt, mirrored
// verbatim from simple-evals/simpleqa_eval.py so our Verdict
// distribution lines up with the published numbers. The judge MUST
// reply with a single letter (A/B/C); anything else is counted as a
// judge failure rather than silently bucketed.
const GradePrompt = `Your job is to look at a question, a gold target, and a predicted answer, and then assign a grade of either ["CORRECT", "INCORRECT", "NOT_ATTEMPTED"].

The following are examples of CORRECT predicted answers.
` + "```" + `
Question: What are the names of Barack Obama's children?
Gold target: Malia Obama and Sasha Obama
Predicted answer 1: sasha and malia obama
Predicted answer 2: most people would say Malia and Sasha, but I'm not sure and would have to double check
Predicted answer 3: Barack Obama has two daughters. Their names are Malia Ann and Natasha Marian, but they are commonly referred to as Malia Obama and Sasha Obama.
` + "```" + `

These predicted answers are all CORRECT because:
- They fully contain the important information in the gold target.
- They do not contain any information that contradicts the gold target.
- Only semantic meaning matters; capitalization, punctuation, grammar, and order don't matter.
- Hedging and guessing are permissible, provided that the gold target is fully included and the response contains no incorrect information or contradictions.

The following are examples of INCORRECT predicted answers.
` + "```" + `
Question: What are the names of Barack Obama's children?
Gold target: Malia and Sasha
Predicted answer 1: Malia.
Predicted answer 2: Malia, Sasha, and Susan.
Predicted answer 3: Barack Obama does not have any children.
Predicted answer 4: I think it's either Malia and Sasha. Or it could be Malia and Jackie.
Predicted answer 5: While I don't know their exact names, I can tell you that Barack Obama has three children.
` + "```" + `

These predicted answers are all INCORRECT because a factual statement in the answer contradicts the gold target. Incorrect statements with hedging (e.g., "it is possible that", "although I'm not sure, I think") are also INCORRECT.

The following are examples of NOT_ATTEMPTED predicted answers.
` + "```" + `
Question: What are the names of Barack Obama's children?
Gold target: Malia and Sasha
Predicted answer 1: I don't know.
Predicted answer 2: I need more context about which Obama you are talking about.
Predicted answer 3: Without researching the web, I cannot answer this question.
Predicted answer 4: Barack Obama has two children. I know that one of them is Malia, but I'm not sure about the other one.
` + "```" + `

These predicted answers are all NOT_ATTEMPTED because the important information in the gold target is not included in the answer and no statement in the answer contradicts the gold target.

Also note:
- For numeric gold targets, the predicted answer must match to the last significant figure; "around 100k" is NOT_ATTEMPTED, "115k" with gold "120k" is CORRECT.
- The gold target may contain more information than the question. The predicted answer only needs to contain what the question asked about.
- Do not punish typos in people's names if the name is clearly the same.

Here is a new example. Simply reply with either CORRECT, INCORRECT, or NOT_ATTEMPTED. Don't apologize or correct yourself; we are just trying to grade the answer.
` + "```" + `
Question: {question}
Gold target: {target}
Predicted answer: {predicted_answer}
` + "```" + `

Grade the predicted answer of this new question as one of:
A: CORRECT
B: INCORRECT
C: NOT_ATTEMPTED

Just return the letters "A", "B", or "C", with no text around it.`

// Options controls a Run.
type Options struct {
	// AnswerLLM is the model under test. Required.
	AnswerLLM llm.LLM
	// JudgeLLM grades the predictions. Required.
	JudgeLLM llm.LLM

	// AnswerPrompt overrides the question template. The literal
	// substring "{question}" is replaced with each question. Default:
	// DefaultAnswerPrompt.
	AnswerPrompt string
	// GradePrompt overrides the judge template. {question}, {target},
	// {predicted_answer} are substituted. Default: GradePrompt.
	GradePrompt string

	// Concurrency caps in-flight LLM calls (answer + judge are paired
	// per question; the cap counts pairs not individual calls).
	Concurrency int

	// LimitQuestions trims the dataset for debug runs. 0 = all.
	LimitQuestions int

	// MaxSamples bounds Report.Samples (one entry per question, in
	// dataset order). Default: 200.
	MaxSamples int

	// PerQuestionTimeout caps a single answer+judge pair. 0 = no
	// timeout (relies on the ambient ctx).
	PerQuestionTimeout time.Duration

	// Hook receives lifecycle events when non-nil.
	Hook EventHook
	// ProgressPct gates intra-run progress events.
	ProgressPct int

	// IncludeTopicBreakdown, when true, populates Report.PerTopic.
	IncludeTopicBreakdown bool
}

// LoadDataset reads a SimpleQA file. Both the upstream CSV format
// (problem, answer, metadata) and our JSONL form (Question{}) are
// auto-detected by extension so a converter pass is optional.
func LoadDataset(path string) (*Dataset, error) {
	ds := &Dataset{Name: filepath.Base(path)}
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".jsonl":
		return ds, loadJSONL(path, ds)
	case ".csv":
		return ds, loadCSV(path, ds)
	default:
		return nil, fmt.Errorf("unsupported extension %q (want .csv or .jsonl)", ext)
	}
}

func loadJSONL(path string, ds *Dataset) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for dec.More() {
		var q Question
		if err := dec.Decode(&q); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		if q.ID == "" {
			q.ID = fmt.Sprintf("q%04d", len(ds.Questions)+1)
		}
		ds.Questions = append(ds.Questions, q)
	}
	return nil
}

// loadCSV parses the upstream simple_qa_test_set.csv. Columns
// (problem, answer, metadata) are positional; the metadata column is
// a JSON-encoded object whose keys we lift into Topic / AnswerType /
// Metadata. Missing columns are tolerated so an upstream column
// addition does not break our loader; missing metadata is the most
// common case.
func loadCSV(path string, ds *Dataset) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	cols := map[string]int{}
	for i, h := range header {
		cols[strings.ToLower(strings.TrimSpace(h))] = i
	}
	problemCol, ok := cols["problem"]
	if !ok {
		return fmt.Errorf("CSV missing required column 'problem'")
	}
	answerCol, ok := cols["answer"]
	if !ok {
		return fmt.Errorf("CSV missing required column 'answer'")
	}
	metaCol, hasMeta := cols["metadata"]

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read row %d: %w", len(ds.Questions)+1, err)
		}
		if problemCol >= len(rec) || answerCol >= len(rec) {
			continue
		}
		q := Question{
			ID:      fmt.Sprintf("q%04d", len(ds.Questions)+1),
			Problem: rec[problemCol],
			Answer:  rec[answerCol],
		}
		if hasMeta && metaCol < len(rec) {
			raw := strings.TrimSpace(rec[metaCol])
			if raw != "" {
				meta := map[string]any{}
				if err := json.Unmarshal([]byte(raw), &meta); err == nil {
					q.Metadata = map[string]string{}
					for k, v := range meta {
						q.Metadata[k] = fmt.Sprint(v)
					}
					if t, ok := meta["topic"].(string); ok {
						q.Topic = t
					}
					if at, ok := meta["answer_type"].(string); ok {
						q.AnswerType = at
					}
				}
			}
		}
		ds.Questions = append(ds.Questions, q)
	}
	return nil
}

// Run scores ds with AnswerLLM (model under test) and JudgeLLM (grader)
// and returns a Report. Concurrency caps pairs of (answer, judge) LLM
// calls so a slow judge cannot stall ingest beyond the cap.
func Run(ctx context.Context, ds *Dataset, opts Options) (*Report, error) {
	if ds == nil {
		return nil, fmt.Errorf("simpleqa: dataset is required")
	}
	if opts.AnswerLLM == nil {
		return nil, fmt.Errorf("simpleqa: Options.AnswerLLM is required")
	}
	if opts.JudgeLLM == nil {
		return nil, fmt.Errorf("simpleqa: Options.JudgeLLM is required")
	}
	if opts.AnswerPrompt == "" {
		opts.AnswerPrompt = DefaultAnswerPrompt
	}
	if opts.GradePrompt == "" {
		opts.GradePrompt = GradePrompt
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.MaxSamples <= 0 {
		opts.MaxSamples = 200
	}

	questions := ds.Questions
	if opts.LimitQuestions > 0 && len(questions) > opts.LimitQuestions {
		questions = questions[:opts.LimitQuestions]
	}

	rep := &Report{
		Dataset:   ds.Name,
		StartedAt: time.Now(),
		Options: map[string]any{
			"concurrency":     opts.Concurrency,
			"limit_questions": opts.LimitQuestions,
			"n_questions":     len(questions),
		},
		PerTopic: map[string]*TopicReport{},
	}
	defer func() { rep.DurationMS = time.Since(rep.StartedAt).Milliseconds() }()

	emit := func(e Event) {
		if opts.Hook == nil {
			return
		}
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		opts.Hook(ctx, e)
	}

	emit(Event{
		Kind:  "start",
		Title: ds.Name,
		Body:  fmt.Sprintf("SimpleQA — %d questions", len(questions)),
		Fields: map[string]string{
			"dataset": ds.Name,
			"n_qs":    fmt.Sprintf("%d", len(questions)),
			"judge":   "configured",
			"answer":  "configured",
		},
	})

	type slot struct {
		Verdict   Verdict
		Predicted string
		JudgeRaw  string
		Failed    bool // judge couldn't be parsed
		Topic     string
		Question  string
		Gold      string
	}
	results := make([]slot, len(questions))

	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var doneCount int64
	total := len(questions)

	var milestones []int64
	if total > 0 && opts.ProgressPct > 0 && opts.Hook != nil {
		for pct := opts.ProgressPct; pct <= 99; pct += opts.ProgressPct {
			ms := int64(total) * int64(pct) / 100
			if ms < 1 {
				ms = 1
			}
			milestones = append(milestones, ms)
		}
	}
	var nextMs int64

	for i, q := range questions {
		i := i
		q := q
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			qctx := ctx
			if opts.PerQuestionTimeout > 0 {
				var cancel context.CancelFunc
				qctx, cancel = context.WithTimeout(ctx, opts.PerQuestionTimeout)
				defer cancel()
			}

			out := slot{Topic: q.Topic, Question: q.Problem, Gold: q.Answer}

			answerPrompt := strings.ReplaceAll(opts.AnswerPrompt, "{question}", q.Problem)
			ans, _, err := opts.AnswerLLM.Generate(qctx, []llm.Message{
				{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: answerPrompt}}},
			})
			if err != nil {
				// Treat answer-LLM failures as NOT_ATTEMPTED: the
				// model didn't produce an answer, so by definition we
				// can't grade it as right or wrong. Counted as a
				// judge_failure so total stays accountable.
				out.Failed = true
				out.Verdict = VerdictNotAttempted
				results[i] = out
				atomic.AddInt64(&doneCount, 1)
				return
			}
			out.Predicted = ans.Content()

			gradePrompt := strings.ReplaceAll(opts.GradePrompt, "{question}", q.Problem)
			gradePrompt = strings.ReplaceAll(gradePrompt, "{target}", q.Answer)
			gradePrompt = strings.ReplaceAll(gradePrompt, "{predicted_answer}", out.Predicted)

			grade, _, err := opts.JudgeLLM.Generate(qctx, []llm.Message{
				{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: gradePrompt}}},
			})
			if err != nil {
				out.Failed = true
				results[i] = out
				atomic.AddInt64(&doneCount, 1)
				return
			}
			out.JudgeRaw = strings.TrimSpace(grade.Content())
			out.Verdict, out.Failed = parseVerdict(out.JudgeRaw)
			results[i] = out

			d := atomic.AddInt64(&doneCount, 1)
			if len(milestones) > 0 {
				idx := atomic.LoadInt64(&nextMs)
				if idx < int64(len(milestones)) && d >= milestones[idx] {
					if atomic.CompareAndSwapInt64(&nextMs, idx, idx+1) {
						pct := int64(opts.ProgressPct) * (idx + 1)
						emit(Event{
							Kind: "qa_progress",
							Body: fmt.Sprintf("%d/%d (~%d%%)", d, total, pct),
							Fields: map[string]string{
								"done":  fmt.Sprintf("%d", d),
								"total": fmt.Sprintf("%d", total),
								"pct":   fmt.Sprintf("%d", pct),
							},
						})
					}
				}
			}
		}()
	}
	wg.Wait()

	// Aggregate.
	for i, s := range results {
		if s.Failed {
			rep.JudgeFailures++
		}
		switch s.Verdict {
		case VerdictCorrect:
			rep.Correct++
		case VerdictIncorrect:
			rep.Incorrect++
		case VerdictNotAttempted:
			rep.NotAttempted++
		}
		if opts.IncludeTopicBreakdown {
			topic := s.Topic
			if topic == "" {
				topic = "(unspecified)"
			}
			tr := rep.PerTopic[topic]
			if tr == nil {
				tr = &TopicReport{}
				rep.PerTopic[topic] = tr
			}
			tr.N++
			switch s.Verdict {
			case VerdictCorrect:
				tr.Correct++
			case VerdictIncorrect:
				tr.Incorrect++
			case VerdictNotAttempted:
				tr.NotAttempted++
			}
		}
		if len(rep.Samples) < opts.MaxSamples {
			rep.Samples = append(rep.Samples, QuestionResult{
				ID:        questions[i].ID,
				Topic:     s.Topic,
				Question:  s.Question,
				Gold:      s.Gold,
				Predicted: s.Predicted,
				Verdict:   s.Verdict,
			})
		}
	}
	rep.N = len(results)
	if rep.N > 0 {
		rep.Accuracy = float64(rep.Correct) / float64(rep.N)
		rep.AbstentionRate = float64(rep.NotAttempted) / float64(rep.N)
		rep.HallucinationRate = float64(rep.Incorrect) / float64(rep.N)
	}
	if attempted := rep.Correct + rep.Incorrect; attempted > 0 {
		rep.AttemptedAccuracy = float64(rep.Correct) / float64(attempted)
	}
	for _, tr := range rep.PerTopic {
		if tr.N > 0 {
			tr.Accuracy = float64(tr.Correct) / float64(tr.N)
			tr.AbstentionRate = float64(tr.NotAttempted) / float64(tr.N)
		}
		if attempted := tr.Correct + tr.Incorrect; attempted > 0 {
			tr.AttemptedAccuracy = float64(tr.Correct) / float64(attempted)
		}
	}

	// Stable topic ordering (for deterministic JSON diffs).
	if opts.IncludeTopicBreakdown && len(rep.PerTopic) > 0 {
		keys := make([]string, 0, len(rep.PerTopic))
		for k := range rep.PerTopic {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}

	emit(Event{
		Kind:  "done",
		Title: ds.Name,
		Body: fmt.Sprintf("acc=%.3f attempted_acc=%.3f abstain=%.3f hallucinate=%.3f",
			rep.Accuracy, rep.AttemptedAccuracy, rep.AbstentionRate, rep.HallucinationRate),
		Fields: map[string]string{
			"accuracy":           fmt.Sprintf("%.3f", rep.Accuracy),
			"attempted_accuracy": fmt.Sprintf("%.3f", rep.AttemptedAccuracy),
			"abstention_rate":    fmt.Sprintf("%.3f", rep.AbstentionRate),
			"hallucination_rate": fmt.Sprintf("%.3f", rep.HallucinationRate),
			"judge_failures":     fmt.Sprintf("%d", rep.JudgeFailures),
			"duration":           time.Since(rep.StartedAt).Round(time.Second).String(),
		},
	})

	return rep, nil
}

// parseVerdict interprets a judge response. The official prompt
// instructs the judge to return a single letter A/B/C, but production
// models occasionally embed it in a sentence ("The answer is A.") or
// reply with the full word ("CORRECT"). We mirror OpenAI's reference
// regex `\b(A|B|C)\b` (word-bounded, case-insensitive) so a stray
// 'A' inside "i AM not sure" is NOT misclassified as CORRECT; only
// truly bounded single-letter tokens count.
//
// Returns failed=true when no recognisable verdict is present — those
// rows do NOT contribute to Correct/Incorrect/NotAttempted aggregates
// (they bump JudgeFailures instead) so the headline accuracy reflects
// only judgements we trust.
func parseVerdict(raw string) (verdict Verdict, failed bool) {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	// Pass 1: bounded letter token. iterate characters and emit a
	// token only on the alpha-non-alpha boundary, so "the A is right"
	// finds "A" but "AM" does not.
	var tok strings.Builder
	flush := func() (Verdict, bool, bool) {
		if tok.Len() == 0 {
			return "", false, false
		}
		s := tok.String()
		tok.Reset()
		if len(s) == 1 {
			switch s[0] {
			case 'A':
				return VerdictCorrect, false, true
			case 'B':
				return VerdictIncorrect, false, true
			case 'C':
				return VerdictNotAttempted, false, true
			}
		}
		return "", false, false
	}
	for _, r := range upper {
		if r >= 'A' && r <= 'Z' {
			tok.WriteRune(r)
			continue
		}
		if v, f, ok := flush(); ok {
			return v, f
		}
	}
	if v, f, ok := flush(); ok {
		return v, f
	}
	// Pass 2: full-word fallback (judge ignored the letter
	// instruction). NOT_ATTEMPTED must be checked before INCORRECT
	// and CORRECT because a single response may contain either
	// substring within the longer phrase ("NOT_ATTEMPTED — clearly
	// not CORRECT"); we want the longest match to win.
	switch {
	case strings.Contains(upper, "NOT_ATTEMPTED") || strings.Contains(upper, "NOT ATTEMPTED"):
		return VerdictNotAttempted, false
	case strings.Contains(upper, "INCORRECT"):
		return VerdictIncorrect, false
	case strings.Contains(upper, "CORRECT"):
		return VerdictCorrect, false
	}
	return "", true
}
