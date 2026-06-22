// Package report owns LoCoMo report JSON types and metric aggregation.
package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
)

const MemoryModeLocalWorkspaceRawSource = "localworkspace_raw_source"

type Task string

const (
	TaskQA     Task = "qa"
	TaskEvent  Task = "event"
	TaskDialog Task = "dialog"
)

type Report struct {
	Dataset          string         `json:"dataset"`
	WorkspaceRoot    string         `json:"workspace_root"`
	MemoryMode       string         `json:"memory_mode"`
	Tasks            []Task         `json:"tasks"`
	StartedAt        time.Time      `json:"started_at"`
	DurationMS       int64          `json:"duration_ms"`
	Partial          bool           `json:"partial,omitempty"`
	CompletedSamples int            `json:"completed_samples"`
	TotalSamples     int            `json:"total_samples"`
	QAMetrics        *QAMetrics     `json:"qa_metrics,omitempty"`
	EventMetrics     *EventMetrics  `json:"event_metrics,omitempty"`
	DialogMetrics    *DialogMetrics `json:"dialog_metrics,omitempty"`
	Samples          []SampleResult `json:"samples,omitempty"`
	Options          map[string]any `json:"options"`
}

type QAMetrics struct {
	N                   int     `json:"n"`
	AverageF1           float64 `json:"average_f1"`
	EvidenceRecallAtK   float64 `json:"evidence_recall_at_k"`
	NoInfoAccuracy      float64 `json:"no_info_accuracy,omitempty"`
	NoInfoCount         int     `json:"no_info_count,omitempty"`
	JudgeAccuracy       float64 `json:"judge_accuracy,omitempty"`
	JudgeEvaluatedCount int     `json:"judge_evaluated_count,omitempty"`
	JudgeCorrectCount   int     `json:"judge_correct_count,omitempty"`
	JudgeIncorrectCount int     `json:"judge_incorrect_count,omitempty"`
	JudgeFailures       int     `json:"judge_failures,omitempty"`
	GenerationFailures  int     `json:"generation_failures"`
}

type EventMetrics struct {
	N                  int     `json:"n"`
	AverageTokenF1     float64 `json:"average_token_f1"`
	AverageRouge1      float64 `json:"average_rouge_1"`
	AverageRougeL      float64 `json:"average_rouge_l"`
	GenerationFailures int     `json:"generation_failures"`
}

type DialogMetrics struct {
	N                  int     `json:"n"`
	AverageBleuLite    float64 `json:"average_bleu_lite"`
	AverageRougeL      float64 `json:"rouge_l"`
	CaptionTermRecall  float64 `json:"caption_term_recall"`
	GenerationFailures int     `json:"generation_failures"`
	TaskName           string  `json:"task_name"`
}

type SampleResult struct {
	ID        string         `json:"id"`
	QAMetrics *QAMetrics     `json:"qa_metrics,omitempty"`
	QA        []QAResult     `json:"qa,omitempty"`
	Events    []EventResult  `json:"events,omitempty"`
	Dialog    []DialogResult `json:"dialog_caption_proxy,omitempty"`
}

type QAResult struct {
	ID             string         `json:"id"`
	Category       string         `json:"category,omitempty"`
	Question       string         `json:"question"`
	Gold           string         `json:"gold"`
	Predicted      string         `json:"predicted"`
	F1             float64        `json:"f1"`
	EvidenceRecall float64        `json:"evidence_recall"`
	HitCounts      *QAHitCounts   `json:"hit_counts,omitempty"`
	Judge          *QAJudgeResult `json:"judge,omitempty"`
	Error          string         `json:"error,omitempty"`
}

type QAHitCounts struct {
	SourceMessages        int `json:"source_messages"`
	SourceDirect          int `json:"source_direct,omitempty"`
	SourceSummaryExpanded int `json:"source_summary_expanded,omitempty"`
	SourceEntityExpanded  int `json:"source_entity_expanded,omitempty"`
	SummaryNode           int `json:"summary_node,omitempty"`
	EntityFact            int `json:"entity_fact,omitempty"`
	DocumentChunk         int `json:"document_chunk,omitempty"`
}

type QAJudgeResult struct {
	Verdict   string `json:"verdict,omitempty"`
	Correct   bool   `json:"correct"`
	Rationale string `json:"rationale,omitempty"`
	Error     string `json:"error,omitempty"`
}

type EventResult struct {
	SessionIndex int     `json:"session_index"`
	Speaker      string  `json:"speaker,omitempty"`
	Gold         string  `json:"gold"`
	Predicted    string  `json:"predicted"`
	TokenF1      float64 `json:"token_f1"`
	Rouge1       float64 `json:"rouge_1"`
	RougeL       float64 `json:"rouge_l"`
	Error        string  `json:"error,omitempty"`
}

type DialogResult struct {
	ID                string  `json:"id"`
	Caption           string  `json:"caption,omitempty"`
	Query             string  `json:"query,omitempty"`
	Gold              string  `json:"gold"`
	Predicted         string  `json:"predicted"`
	BleuLite          float64 `json:"bleu_lite"`
	RougeL            float64 `json:"rouge_l"`
	CaptionTermRecall float64 `json:"caption_term_recall"`
	Error             string  `json:"error,omitempty"`
}

func New(datasetName, workspaceRoot string, tasks []Task, totalSamples int, options map[string]any, tasksEnabled map[Task]bool) *Report {
	rep := &Report{
		Dataset:       datasetName,
		WorkspaceRoot: workspaceRoot,
		MemoryMode:    MemoryModeLocalWorkspaceRawSource,
		Tasks:         append([]Task(nil), tasks...),
		StartedAt:     time.Now(),
		TotalSamples:  totalSamples,
		Options:       options,
	}
	if tasksEnabled[TaskQA] {
		rep.QAMetrics = &QAMetrics{}
	}
	if tasksEnabled[TaskEvent] {
		rep.EventMetrics = &EventMetrics{}
	}
	if tasksEnabled[TaskDialog] {
		rep.DialogMetrics = &DialogMetrics{TaskName: "caption_proxy_multimodal_dialog_generation"}
	}
	return rep
}

func AccumulateQA(m *QAMetrics, item dataset.QAItem, row QAResult) {
	if m == nil {
		return
	}
	m.N++
	if row.Error != "" {
		m.GenerationFailures++
		return
	}
	m.AverageF1 += row.F1
	m.EvidenceRecallAtK += row.EvidenceRecall
	accumulateQAJudge(m, row.Judge)
	if item.CategoryID == 5 || item.Category == "adversarial" {
		m.NoInfoCount++
		if isNoInfoAnswer(row.Predicted) {
			m.NoInfoAccuracy++
		}
	}
}

func accumulateQAJudge(m *QAMetrics, judge *QAJudgeResult) {
	if m == nil || judge == nil {
		return
	}
	if judge.Error != "" {
		m.JudgeFailures++
		return
	}
	m.JudgeEvaluatedCount++
	if judge.Correct {
		m.JudgeCorrectCount++
		m.JudgeAccuracy++
		return
	}
	m.JudgeIncorrectCount++
}

func AccumulateEvent(m *EventMetrics, row EventResult) {
	if m == nil {
		return
	}
	m.N++
	if row.Error != "" {
		m.GenerationFailures++
		return
	}
	m.AverageTokenF1 += row.TokenF1
	m.AverageRouge1 += row.Rouge1
	m.AverageRougeL += row.RougeL
}

func AccumulateDialog(m *DialogMetrics, row DialogResult) {
	if m == nil {
		return
	}
	m.N++
	if row.Error != "" {
		m.GenerationFailures++
		return
	}
	m.AverageBleuLite += row.BleuLite
	m.AverageRougeL += row.RougeL
	m.CaptionTermRecall += row.CaptionTermRecall
}

func Finalize(rep *Report) {
	if rep == nil {
		return
	}
	FinalizeQA(rep.QAMetrics)
	if rep.EventMetrics != nil && rep.EventMetrics.N > 0 {
		n := float64(rep.EventMetrics.N - rep.EventMetrics.GenerationFailures)
		if n > 0 {
			rep.EventMetrics.AverageTokenF1 /= n
			rep.EventMetrics.AverageRouge1 /= n
			rep.EventMetrics.AverageRougeL /= n
		}
	}
	if rep.DialogMetrics != nil && rep.DialogMetrics.N > 0 {
		n := float64(rep.DialogMetrics.N - rep.DialogMetrics.GenerationFailures)
		if n > 0 {
			rep.DialogMetrics.AverageBleuLite /= n
			rep.DialogMetrics.AverageRougeL /= n
			rep.DialogMetrics.CaptionTermRecall /= n
		}
	}
}

func FinalizeQA(m *QAMetrics) {
	if m == nil || m.N == 0 {
		return
	}
	n := float64(m.N - m.GenerationFailures)
	if n > 0 {
		m.AverageF1 /= n
		m.EvidenceRecallAtK /= n
	}
	if m.NoInfoCount > 0 {
		m.NoInfoAccuracy /= float64(m.NoInfoCount)
	}
	if m.JudgeEvaluatedCount > 0 {
		m.JudgeAccuracy /= float64(m.JudgeEvaluatedCount)
	}
}

func Snapshot(rep *Report, partial bool) *Report {
	if rep == nil {
		return nil
	}
	out := *rep
	out.DurationMS = time.Since(rep.StartedAt).Milliseconds()
	out.Partial = partial
	out.Tasks = append([]Task(nil), rep.Tasks...)
	out.QAMetrics = cloneQAMetrics(rep.QAMetrics)
	out.EventMetrics = cloneEventMetrics(rep.EventMetrics)
	out.DialogMetrics = cloneDialogMetrics(rep.DialogMetrics)
	out.Samples = cloneSampleResults(rep.Samples)
	out.Options = cloneReportOptions(rep.Options)
	Finalize(&out)
	return &out
}

func cloneQAMetrics(in *QAMetrics) *QAMetrics {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneEventMetrics(in *EventMetrics) *EventMetrics {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneDialogMetrics(in *DialogMetrics) *DialogMetrics {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneSampleResults(in []SampleResult) []SampleResult {
	if in == nil {
		return nil
	}
	out := make([]SampleResult, len(in))
	for i, sample := range in {
		out[i] = sample
		out[i].QAMetrics = cloneQAMetrics(sample.QAMetrics)
		out[i].QA = append([]QAResult(nil), sample.QA...)
		out[i].Events = append([]EventResult(nil), sample.Events...)
		out[i].Dialog = append([]DialogResult(nil), sample.Dialog...)
	}
	return out
}

func cloneReportOptions(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func SummaryLine(rep *Report) string {
	parts := []string{}
	if rep.QAMetrics != nil {
		parts = append(parts, fmt.Sprintf("qa_f1=%.3f", rep.QAMetrics.AverageF1))
	}
	if rep.EventMetrics != nil {
		parts = append(parts, fmt.Sprintf("event_rouge_l=%.3f", rep.EventMetrics.AverageRougeL))
	}
	if rep.DialogMetrics != nil {
		parts = append(parts, fmt.Sprintf("caption_proxy_bleu=%.3f", rep.DialogMetrics.AverageBleuLite))
	}
	return strings.Join(parts, " ")
}

func isNoInfoAnswer(s string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range []string{
		"not mentioned",
		"no information available",
		"not enough information",
		"cannot answer",
		"unknown",
		"i don't know",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
