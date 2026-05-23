package locomo

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/env"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const answerVerifierPrompt = `You are auditing a retrieval-grounded QA answer.

Decide WHY the prediction failed or whether it is actually supported by the retrieved memories.
Do not grade from world knowledge. Use only QUESTION, GOLD ANSWERS, PREDICTION, and TOP MEMORIES.

Verdict labels:
- supported_but_judge_failed: prediction is supported by memories and appears compatible with at least one gold answer.
- ignored_strong_evidence: top memories contain clear evidence for the gold answer, but prediction ignores it.
- wrong_temporal_numeric_reasoning: memories contain the needed date/number/count/duration but prediction chooses or computes it incorrectly.
- distractor_dominated: prediction follows a plausible distractor memory while stronger gold-supporting evidence is also present.
- evidence_insufficient: top memories do not contain enough evidence to answer the question.
- gold_surface_missing: memories may be semantically related, but the literal gold answer surface is absent or too paraphrased.
- unsupported_prediction: prediction is not supported or is contradicted by the memories.

Suggested fix labels:
- retrieval
- ranking
- context_rendering
- answer_reasoning
- temporal_numeric_tooling
- insufficient_evidence
- judge_review

Return strict JSON only.`

var answerVerifierSchema = llm.JSONSchemaParam{
	Name:        "answer_grounding_verdict",
	Description: "Grounded answer failure attribution",
	Strict:      true,
	Schema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"verdict",
			"suggested_fix",
			"supporting_ranks",
			"strong_evidence_ranks",
			"reason",
		},
		"properties": map[string]any{
			"verdict": map[string]any{
				"type": "string",
				"enum": []string{
					"supported_but_judge_failed",
					"ignored_strong_evidence",
					"wrong_temporal_numeric_reasoning",
					"distractor_dominated",
					"evidence_insufficient",
					"gold_surface_missing",
					"unsupported_prediction",
				},
			},
			"suggested_fix": map[string]any{
				"type": "string",
				"enum": []string{
					"retrieval",
					"ranking",
					"context_rendering",
					"answer_reasoning",
					"temporal_numeric_tooling",
					"insufficient_evidence",
					"judge_review",
				},
			},
			"supporting_ranks":      map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			"strong_evidence_ranks": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			"reason":                map[string]any{"type": "string"},
		},
	},
}

type answerVerifyRecord struct {
	QID                 string   `json:"qid"`
	Query               string   `json:"query"`
	Tags                []string `json:"tags,omitempty"`
	Judge               float64  `json:"judge"`
	Prediction          string   `json:"prediction"`
	GoldAnswers         []string `json:"gold_answers,omitempty"`
	Verdict             string   `json:"verdict"`
	SuggestedFix        string   `json:"suggested_fix"`
	SupportingRanks     []int    `json:"supporting_ranks,omitempty"`
	StrongEvidenceRanks []int    `json:"strong_evidence_ranks,omitempty"`
	Reason              string   `json:"reason"`
	Error               string   `json:"error,omitempty"`
}

func addLocomoVerifyAnswer(parent *cobra.Command) {
	var (
		verifierSpec string
		outPath      string
		onlyMisses   bool
		limit        int
		concurrency  int
		timeout      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "verify-answer <report.json> <answer-replay.jsonl>",
		Short: "Offline LLM audit of whether answer misses ignored, lacked, or misused recalled evidence",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			report, err := loadReport(args[0])
			if err != nil {
				return err
			}
			replays, err := loadAnswerReplayDump(args[1])
			if err != nil {
				return err
			}
			verifier, err := env.BuildLLM(verifierSpec)
			if err != nil {
				return fmt.Errorf("--verifier-llm: %w", err)
			}
			if verifier == nil {
				return fmt.Errorf("--verifier-llm is required")
			}
			records := answerReplayRecordsForVerification(report, replays, onlyMisses, limit)
			var w io.Writer = os.Stdout
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			return verifyAnswerRecords(c.Context(), verifier, records, verifyAnswerOptions{
				Concurrency: concurrency,
				Timeout:     timeout,
				Writer:      w,
			})
		},
	}
	cmd.Flags().StringVar(&verifierSpec, "verifier-llm", "azure_gpt54", "LLM alias for grounded verifier")
	cmd.Flags().StringVar(&outPath, "out", "", "output JSONL path; defaults to stdout")
	cmd.Flags().BoolVar(&onlyMisses, "only-misses", true, "only verify questions whose report judge is 0")
	cmd.Flags().IntVar(&limit, "limit", 0, "verify at most N records after filtering (0 = all)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4, "parallel verifier calls")
	cmd.Flags().DurationVar(&timeout, "timeout", 90*time.Second, "per-record verifier timeout")
	parent.AddCommand(cmd)
}

func loadAnswerReplayDump(path string) (map[string]AnswerReplayRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]AnswerReplayRecord{}
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 64*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec AnswerReplayRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, err
		}
		out[rec.QID] = rec
	}
	return out, sc.Err()
}

func answerReplayRecordsForVerification(report *Report, replays map[string]AnswerReplayRecord, onlyMisses bool, limit int) []AnswerReplayRecord {
	out := make([]AnswerReplayRecord, 0, len(report.PerQuestion))
	for _, q := range report.PerQuestion {
		if onlyMisses && q.Judge >= 0.5 {
			continue
		}
		rec, ok := replays[q.ID]
		if !ok {
			continue
		}
		rec.Outcome.Prediction = q.Prediction
		rec.Outcome.EM = q.EM
		rec.Outcome.F1 = q.F1
		rec.Outcome.Judge = q.Judge
		rec.Tags = append([]string(nil), q.Tags...)
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

type verifyAnswerOptions struct {
	Concurrency int
	Timeout     time.Duration
	Writer      io.Writer
}

func verifyAnswerRecords(ctx context.Context, verifier llm.LLM, records []AnswerReplayRecord, opts verifyAnswerOptions) error {
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > len(records) && len(records) > 0 {
		conc = len(records)
	}
	enc := json.NewEncoder(opts.Writer)
	var encMu sync.Mutex
	jobs := make(chan AnswerReplayRecord)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rec := range jobs {
				out := verifyOneAnswer(ctx, verifier, rec, opts.Timeout)
				encMu.Lock()
				if err := enc.Encode(out); err != nil {
					setErr(err)
				}
				encMu.Unlock()
			}
		}()
	}
	for _, rec := range records {
		jobs <- rec
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func verifyOneAnswer(ctx context.Context, verifier llm.LLM, rec AnswerReplayRecord, timeout time.Duration) answerVerifyRecord {
	out := answerVerifyRecord{
		QID:         rec.QID,
		Query:       rec.Query,
		Tags:        append([]string(nil), rec.Tags...),
		Judge:       rec.Outcome.Judge,
		Prediction:  rec.Outcome.Prediction,
		GoldAnswers: append([]string(nil), rec.GoldAnswers...),
	}
	vctx := ctx
	cancel := func() {}
	if timeout > 0 {
		vctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	resp, _, err := verifier.Generate(vctx, []llm.Message{
		{Role: model.RoleSystem, Parts: []model.Part{{Type: model.PartText, Text: answerVerifierPrompt}}},
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: buildAnswerVerifierUserMessage(rec)}}},
	}, llm.WithJSONSchema(answerVerifierSchema), llm.WithJSONMode(true), llm.WithTemperature(0))
	if err != nil {
		out.Error = err.Error()
		return out
	}
	payload, _, err := llm.ExtractJSON(resp.Content())
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var parsed struct {
		Verdict             string `json:"verdict"`
		SuggestedFix        string `json:"suggested_fix"`
		SupportingRanks     []int  `json:"supporting_ranks"`
		StrongEvidenceRanks []int  `json:"strong_evidence_ranks"`
		Reason              string `json:"reason"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		out.Error = err.Error()
		return out
	}
	out.Verdict = parsed.Verdict
	out.SuggestedFix = parsed.SuggestedFix
	out.SupportingRanks = parsed.SupportingRanks
	out.StrongEvidenceRanks = parsed.StrongEvidenceRanks
	out.Reason = parsed.Reason
	return out
}

func buildAnswerVerifierUserMessage(rec AnswerReplayRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "QUESTION: %s\n", rec.Query)
	if rec.AskedAt != "" {
		fmt.Fprintf(&b, "ASKED_AT: %s\n", rec.AskedAt)
	}
	fmt.Fprintf(&b, "GOLD ANSWERS: %s\n", strings.Join(rec.GoldAnswers, " | "))
	fmt.Fprintf(&b, "PREDICTION: %s\n", rec.Outcome.Prediction)
	fmt.Fprintf(&b, "JUDGE: %.3f\n", rec.Outcome.Judge)
	b.WriteString("\nTOP MEMORIES:\n")
	for _, h := range rec.Hits {
		fmt.Fprintf(&b, "[#%d]", h.Rank)
		if h.Kind != "" {
			fmt.Fprintf(&b, " kind=%s", h.Kind)
		}
		if h.ValidFrom != "" {
			fmt.Fprintf(&b, " valid_from=%s", h.ValidFrom)
		}
		if len(h.EvidenceIDs) > 0 {
			fmt.Fprintf(&b, " evidence_ids=%s", strings.Join(h.EvidenceIDs, ","))
		}
		fmt.Fprintf(&b, "\n%s\n", h.Content)
	}
	return b.String()
}
