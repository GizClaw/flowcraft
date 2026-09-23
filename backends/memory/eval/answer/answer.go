// Package answer implements the model-backed answering and grading stages of
// the memory eval harness: it answers a question from recalled context and
// judges the answer against the dataset's expectations.
package answer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// Runtime is the narrow inference surface the model stages need.
type Runtime interface {
	Generate(context.Context, model.ModelRef, inference.GenerateRequest) (inference.GenerateResponse, error)
}

// Model answers questions from recalled context and judges the answers with
// the same generation model. Both eval.Answerer and eval.Judge are satisfied.
type Model struct {
	runtime         Runtime
	ref             model.ModelRef
	maxContextRunes int
	judgeStyle      JudgeStyle
	answerStyle     AnswerStyle
	calls           atomic.Int64
	inputTokens     atomic.Int64
	outputTokens    atomic.Int64
}

// JudgeStyle selects which grading prompt the judge uses.
type JudgeStyle string

const (
	// JudgeStrict requires the prediction to state the gold's key information
	// precisely (vague paraphrase of a concrete fact is incorrect).
	JudgeStrict JudgeStyle = "strict"
	// JudgeLocoMo mirrors the lenient prompt used by common LoCoMo
	// leaderboard harnesses (topic-level matches count, date formats are
	// normalized). Use it for cross-paper comparability, not as product
	// semantics.
	JudgeLocoMo JudgeStyle = "locomo"
)

// New builds a model-backed answerer/judge.
func New(runtime Runtime, ref model.ModelRef) (*Model, error) {
	if runtime == nil {
		return nil, errors.New("memory eval answer: runtime is required")
	}
	if strings.TrimSpace(ref.ID.Provider) == "" || strings.TrimSpace(ref.ID.Name) == "" {
		return nil, errors.New("memory eval answer: model provider and name are required")
	}
	return &Model{
		runtime: runtime, ref: ref, maxContextRunes: 24_000,
		judgeStyle: JudgeStrict, answerStyle: AnswerStyleLong,
	}, nil
}

// WithMaxContextRunes bounds how many runes of recalled context are rendered
// into one prompt. Zero or negative keeps the current limit.
func (model *Model) WithMaxContextRunes(runes int) *Model {
	if model != nil && runes > 0 {
		model.maxContextRunes = runes
	}
	return model
}

// WithAnswerStyle selects the answering protocol. Unknown values keep the
// current style.
func (model *Model) WithAnswerStyle(style AnswerStyle) *Model {
	if model != nil && (style == AnswerStyleLong || style == AnswerStyleShort) {
		model.answerStyle = style
	}
	return model
}

// System returns the answering prompt this model uses, so a run can record its
// version in the fingerprint.
func (model *Model) System() string {
	if model != nil && model.answerStyle == AnswerStyleShort {
		return answerShortSystem
	}
	return answerSystem
}

// WithJudgeStyle selects the grading prompt. Unknown values keep the current
// style.
func (model *Model) WithJudgeStyle(style JudgeStyle) *Model {
	if model != nil && (style == JudgeStrict || style == JudgeLocoMo) {
		model.judgeStyle = style
	}
	return model
}

// Stats reports cumulative model usage across answers and judgments.
func (model *Model) Stats() (calls, inputTokens, outputTokens int64) {
	if model == nil {
		return 0, 0, 0
	}
	return model.calls.Load(), model.inputTokens.Load(), model.outputTokens.Load()
}

// Answer generates one answer from the recalled items.
func (model *Model) Answer(ctx context.Context, question eval.Question, items []corememory.ContextItem) (string, error) {
	if model == nil || model.runtime == nil {
		return "", errors.New("memory eval answer: model is incomplete")
	}
	if ctx == nil {
		return "", errors.New("memory eval answer: context is required")
	}
	if strings.TrimSpace(question.Query) == "" {
		return "", errors.New("memory eval answer: question is required")
	}
	prompt := answerUser(question, renderContext(items, model.maxContextRunes))
	if model.answerStyle == AnswerStyleShort {
		// The official protocol cues brevity with a trailing "Short answer:".
		prompt += "\n\nShort answer:"
	}
	request := generateRequest(model.System(), prompt, nil)
	response, err := model.runtime.Generate(ctx, model.ref, request)
	if err != nil {
		return "", fmt.Errorf("memory eval answer: generate: %w", err)
	}
	model.record(response)
	answer := strings.TrimSpace(response.Message.Content.Text())
	if answer == "" {
		return "", errors.New("memory eval answer: model returned an empty answer")
	}
	return answer, nil

}

// Judge grades one generated answer against the question's expectations.
func (model *Model) Judge(ctx context.Context, question eval.Question, candidate string) (bool, error) {
	if model == nil || model.runtime == nil {
		return false, errors.New("memory eval answer: model is incomplete")
	}
	if ctx == nil {
		return false, errors.New("memory eval answer: context is required")
	}
	request := generateRequest(
		model.judgeStyle.system(),
		judgeUser(question, candidate),
		nil,
	)
	response, err := model.runtime.Generate(ctx, model.ref, request)
	if err != nil {
		return false, fmt.Errorf("memory eval answer: judge: %w", err)
	}
	model.record(response)
	return parseVerdict(response.Message.Content.Text())
}

func (model *Model) record(response inference.GenerateResponse) {
	model.calls.Add(1)
	model.inputTokens.Add(response.Usage.InputTokens)
	model.outputTokens.Add(response.Usage.OutputTokens)
}

func (style JudgeStyle) system() string {
	if style == JudgeLocoMo {
		return judgeSystemLocoMo
	}
	return judgeSystem
}

func generateRequest(system, user string, response *inference.ResponseFormat) inference.GenerateRequest {
	intent := inference.Intent{Text: &inference.TextIntent{Response: response}}
	return inference.GenerateRequest{
		Context: []coremessage.Message{{
			Role:    coremessage.RoleSystem,
			Content: coremessage.NewTextContent(system),
		}},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: coremessage.NewTextContent(user),
				Intent:  intent,
			},
		},
	}
}

// renderContext renders recalled items in retrieval order with the
// [source-class/kind] labels the memory eval protocol uses, capped to
// maxRunes so one pathological item cannot dominate the prompt.
func renderContext(items []corememory.ContextItem, maxRunes int) string {
	var builder strings.Builder
	remaining := maxRunes
	for index, item := range items {
		text := strings.TrimSpace(item.Content.Text())
		if text == "" {
			continue
		}
		if remaining <= 0 {
			break
		}
		if runes := []rune(text); len(runes) > remaining {
			text = string(runes[:remaining])
		}
		remaining -= utf8.RuneCountInString(text)
		fmt.Fprintf(&builder, "%d. [%s/%s]\n%s\n\n", index+1, item.SourceClass, item.Kind, text)
	}
	return strings.TrimSpace(builder.String())
}

func answerUser(question eval.Question, context string) string {
	var builder strings.Builder
	if askedAt := strings.TrimSpace(question.AskedAt); askedAt != "" {
		fmt.Fprintf(&builder, "The question was asked at: %s\n", askedAt)
	}
	fmt.Fprintf(&builder, "Question: %s\n", strings.TrimSpace(question.Query))
	fmt.Fprintf(&builder, "\nMemories:\n%s\n", context)
	return builder.String()
}

func judgeUser(question eval.Question, candidate string) string {
	wants := make([]string, 0, len(question.WantContains))
	for _, want := range question.WantContains {
		if trimmed := strings.TrimSpace(want); trimmed != "" {
			wants = append(wants, trimmed)
		}
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Question: %s\n", strings.TrimSpace(question.Query))
	builder.WriteString("Gold:\n")
	for _, want := range wants {
		fmt.Fprintf(&builder, "- %s\n", want)
	}
	fmt.Fprintf(&builder, "Prediction:\n%s\n", strings.TrimSpace(candidate))
	return builder.String()
}

// verdictJSONRE matches the structured verdict the leaderboard-style prompt
// asks for.
var verdictJSONRE = regexp.MustCompile(`(?i)"correct"\s*:\s*(true|false)`)

// parseVerdict reads the judge's verdict. Only two shapes are accepted: the
// JSON boolean the lenient prompt asks for, or the bare CORRECT / INCORRECT
// token the strict prompt asks for. Anything else is rejected rather than
// guessed at, because scanning prose for the word "correct" silently accepted
// negations and hedges -- "the prediction is not fully correct" used to score
// as CORRECT, which inflates the answer rate.
func parseVerdict(raw string) (bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false, errors.New("memory eval answer: empty judge verdict")
	}
	if matches := verdictJSONRE.FindAllStringSubmatch(trimmed, -1); len(matches) > 0 {
		// The lenient prompt asks for reasoning first and the verdict last;
		// taking the first match could read a schema example in the reasoning.
		last := matches[len(matches)-1]
		return strings.EqualFold(last[1], "true"), nil
	}
	switch strings.ToLower(strings.Trim(trimmed, " \t\r\n.\"'*`")) {
	case "correct":
		return true, nil
	case "incorrect":
		return false, nil
	}
	return false, fmt.Errorf("memory eval answer: unrecognized judge verdict %q", clipRunes(trimmed, 80))
}

func clipRunes(value string, max int) string {
	runes := []rune(value)
	if max <= 0 || len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

// AnswerPromptVersion names the answer policy below. Bump it whenever the
// prompt changes: it is part of the run fingerprint, so a resumed run cannot
// silently mix answers produced under two different policies.
const AnswerPromptVersion = "answer-policy-v2"

// AnswerStyle selects the answering protocol. Short mirrors the official
// LoCoMo protocol ("write an answer in the form of a short phrase ... Short
// answer:"), which is what token-F1 grading expects; Long is the product
// protocol used for the strict/lenient numbers.
type AnswerStyle string

const (
	AnswerStyleLong  AnswerStyle = "long"
	AnswerStyleShort AnswerStyle = "short"
)

// AnswerShortPromptVersion names the short-answer prompt; it is recorded in the
// run fingerprint like the long-answer policy.
const AnswerShortPromptVersion = "answer-short-v1"

// JudgePromptVersion names the grading prompts. v2 hands the question to the
// judge: the strict rubric rejects an answer that "answers a different
// question", which could not be enforced while the judge saw only gold and
// prediction.
const JudgePromptVersion = "judge-v2"

// The answer and judge prompts mirror the memory eval protocol used by the
// pre-rebuild eval harness: the rules matter for list questions, exact
// figures, and partial evidence.
//
// v2 splits "grounding" from "interpretation": the memories must carry every
// claim about the people involved, but general knowledge may bridge them (a
// date to the holiday it falls on, a game to its console). It also requires a
// committed answer when the evidence points somewhere, because hedging with
// "not mentioned" was the single largest failure mode on open-domain
// questions.
// answerShortSystem mirrors the official LoCoMo answering prompt, which is the
// protocol its token-F1 metric grades.
const answerShortSystem = `Write an answer in the form of a short phrase, using exact words from the memories whenever possible. Reply with the short answer only: no explanation, no full sentences, no restating the question.`

const answerSystem = `Answer the question from the memories below.

Rules:
- Ground every claim about the people, events, places, and their attributes in the memories. Never invent such facts.
- You may use general world knowledge to interpret what the memories say: what a date coincides with, what a game or product is, what a term means. Interpretation is allowed; inventing facts about these people is not.
- When the memories support a likely conclusion without stating it outright, give the conclusion and mark it as likely (for example "Likely no, because the trip went badly"). Do not end with "not mentioned" or "I don't know" while relevant evidence is present.
- When the question asks for a list (activities, events, items, books, places, people, etc.), list every item the memories mention, in the concrete words they use. Never replace a concrete item with a general word (say "Sweden", not "her home country").
- Keep names, dates, places, titles, and numbers exactly as stated.
- Answer "I don't know" only when the memories contain nothing relevant.`

const judgeSystem = `You grade whether a prediction correctly answers a gold answer. Answer with exactly one word: CORRECT or INCORRECT.

Rules:
- CORRECT means the prediction contains all key information in the gold answer(s). Extra details or different wording are allowed.
- INCORRECT means the prediction misses any key information, contradicts the gold, or answers a different question.
- If the gold is a list of items, the prediction must mention every item to be CORRECT.
- If the gold is a single concrete fact (name, place, date, number, or a "Yes"/"No" answer), the prediction must state it precisely; a vague paraphrase is INCORRECT.`

// judgeSystemLocoMo mirrors the prompt used by common LoCoMo leaderboard
// harnesses: generous topic-level matching and date-format normalization,
// with a one-sentence reasoning step before the verdict.
const judgeSystemLocoMo = `Your task is to label an answer to a question as 'CORRECT' or 'WRONG'. You will be given a question, a gold (ground truth) answer, and a generated answer.

The point of the question is to ask about something one user should know about the other user based on their prior conversations. The gold answer is usually a concise answer that includes the referenced topic.

Be generous with your grading - as long as the generated answer touches on the same topic as the gold answer, it should be counted as CORRECT.

For time related questions, be generous as well: as long as the answer refers to the same date or time period as the gold answer, count it as CORRECT, even if the format differs (for example "May 7th" vs "7 May") or the answer uses a longer phrasing.

First, provide a short (one sentence) explanation of your reasoning, then return your verdict as JSON: {"correct": true} if CORRECT, otherwise {"correct": false}.`
