package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
)

func TestAccumulateFinalizeAndSnapshot(t *testing.T) {
	rep := New("synthetic", "/tmp/work", []Task{TaskQA, TaskEvent, TaskDialog}, 2, map[string]any{"qa_top_k": 20}, map[Task]bool{
		TaskQA:     true,
		TaskEvent:  true,
		TaskDialog: true,
	})
	row := QAResult{
		ID:             "qa_0001",
		Question:       "What does Ada like?",
		Gold:           "tea",
		Predicted:      "tea",
		F1:             1,
		EvidenceRecall: 1,
		Judge:          &QAJudgeResult{Verdict: "correct", Correct: true},
	}
	AccumulateQA(rep.QAMetrics, dataset.QAItem{Question: row.Question, Answer: row.Gold}, row)
	rep.Samples = append(rep.Samples, SampleResult{ID: "conv-a", QAMetrics: &QAMetrics{}})
	AccumulateQA(rep.Samples[0].QAMetrics, dataset.QAItem{Question: row.Question, Answer: row.Gold}, row)
	FinalizeQA(rep.Samples[0].QAMetrics)
	rep.CompletedSamples = 1

	snap := Snapshot(rep, true)
	if !snap.Partial || snap.CompletedSamples != 1 || snap.TotalSamples != 2 {
		t.Fatalf("snapshot progress = partial:%t completed:%d total:%d", snap.Partial, snap.CompletedSamples, snap.TotalSamples)
	}
	if snap.QAMetrics == nil || snap.QAMetrics.AverageF1 != 1 || snap.QAMetrics.JudgeAccuracy != 1 {
		t.Fatalf("snapshot QA metrics = %+v, want finalized aggregate", snap.QAMetrics)
	}
	if snap.Samples[0].QAMetrics == nil || snap.Samples[0].QAMetrics.AverageF1 != 1 {
		t.Fatalf("snapshot sample QA metrics = %+v, want finalized sample", snap.Samples[0].QAMetrics)
	}
}

func TestReportJSONDoesNotIncludeLegacyJudgeAverageScore(t *testing.T) {
	rep := New("synthetic", "/tmp/work", []Task{TaskQA}, 1, map[string]any{}, map[Task]bool{TaskQA: true})
	AccumulateQA(rep.QAMetrics, dataset.QAItem{}, QAResult{
		F1:             1,
		EvidenceRecall: 1,
		Judge:          &QAJudgeResult{Verdict: "correct", Correct: true},
	})
	Finalize(rep)
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "judge_average_score") {
		t.Fatalf("report JSON contains legacy judge_average_score: %s", data)
	}
	if !strings.Contains(string(data), "judge_accuracy") {
		t.Fatalf("report JSON missing judge_accuracy: %s", data)
	}
}
