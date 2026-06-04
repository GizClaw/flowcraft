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
		verifierSpec        string
		outPath             string
		auditPath           string
		onlyMisses          bool
		secondaryMissFilter string
		tagFilter           string
		qidFilter           string
		limit               int
		concurrency         int
		timeout             time.Duration
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
			auditRows, err := loadAnswerAuditRows(auditPath)
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
			records := answerReplayRecordsForVerification(report, replays, answerReplayFilter{
				OnlyMisses:      onlyMisses,
				Limit:           limit,
				Tags:            csvSet(tagFilter),
				QIDs:            csvSet(qidFilter),
				SecondaryMisses: csvSet(secondaryMissFilter),
				AuditRows:       auditRows,
			})
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
	cmd.Flags().StringVar(&auditPath, "audit", "", "optional analyze-recall JSONL path used for secondary_miss filtering")
	cmd.Flags().BoolVar(&onlyMisses, "only-misses", true, "only verify questions whose report judge is 0")
	cmd.Flags().StringVar(&secondaryMissFilter, "secondary-miss", "", "comma-separated secondary_miss values to replay, e.g. answer_miss_temporal_or_numeric_reasoning")
	cmd.Flags().StringVar(&tagFilter, "tags", "", "comma-separated question tags/categories to replay")
	cmd.Flags().StringVar(&qidFilter, "qids", "", "comma-separated question ids to replay")
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

type answerReplayFilter struct {
	OnlyMisses      bool
	Limit           int
	Tags            map[string]struct{}
	QIDs            map[string]struct{}
	SecondaryMisses map[string]struct{}
	AuditRows       map[string]answerAuditRow
}

type answerAuditRow struct {
	QID           string `json:"qid"`
	MissType      string `json:"miss_type"`
	SecondaryMiss string `json:"secondary_miss"`
}

func loadAnswerAuditRows(path string) (map[string]answerAuditRow, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]answerAuditRow{}
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 64*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row answerAuditRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		if row.QID != "" {
			out[row.QID] = row
		}
	}
	return out, sc.Err()
}

func answerReplayRecordsForVerification(report *Report, replays map[string]AnswerReplayRecord, filter answerReplayFilter) []AnswerReplayRecord {
	out := make([]AnswerReplayRecord, 0, len(report.PerQuestion))
	for _, q := range report.PerQuestion {
		if filter.OnlyMisses && q.Judge >= 0.5 {
			continue
		}
		if len(filter.QIDs) > 0 && !setContains(filter.QIDs, q.ID) {
			continue
		}
		if len(filter.Tags) > 0 && !anySetContains(filter.Tags, q.Tags) {
			continue
		}
		if len(filter.SecondaryMisses) > 0 {
			row, ok := filter.AuditRows[q.ID]
			if !ok || !setContains(filter.SecondaryMisses, row.SecondaryMiss) {
				continue
			}
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
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out
}

func csvSet(raw string) map[string]struct{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out[part] = struct{}{}
	}
	return out
}

func setContains(set map[string]struct{}, value string) bool {
	_, ok := set[value]
	return ok
}

func anySetContains(set map[string]struct{}, values []string) bool {
	for _, value := range values {
		if setContains(set, value) {
			return true
		}
	}
	return false
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
	if strings.TrimSpace(rec.AnswerBody) != "" {
		b.WriteString("\nANSWER_CONTEXT_BODY:\n")
		b.WriteString(rec.AnswerBody)
		if !strings.HasSuffix(rec.AnswerBody, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("\nTOP MEMORIES:\n")
	for _, artifact := range rec.RecallArtifacts {
		fmt.Fprintf(&b, "[#%d]", artifact.Rank)
		if artifact.Kind != "" {
			fmt.Fprintf(&b, " kind=%s", artifact.Kind)
		}
		if artifact.ValidFrom != "" {
			fmt.Fprintf(&b, " valid_from=%s", artifact.ValidFrom)
		}
		if len(artifact.EvidenceIDs) > 0 {
			fmt.Fprintf(&b, " evidence_ids=%s", strings.Join(artifact.EvidenceIDs, ","))
		}
		fmt.Fprintf(&b, "\n%s\n", artifact.Content)
	}
	return b.String()
}
