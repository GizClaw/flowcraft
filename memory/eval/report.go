package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/eval/scenarios"
)

// Report is the JSON artifact of one eval run.
type Report struct {
	Suite         string                    `json:"suite"`
	Dataset       string                    `json:"dataset"`
	Host          string                    `json:"host,omitempty"`
	Profile       string                    `json:"profile"`
	PolicyDigest  string                    `json:"policy_digest"`
	GenerateModel string                    `json:"generate_model"`
	EmbedModel    string                    `json:"embed_model"`
	AnswerModel   string                    `json:"answer_model"`
	JudgeModel    string                    `json:"judge_model,omitempty"`
	Budget        budget                    `json:"budget"`
	N             int                       `json:"n"`
	Aggregate     scenarios.ScoreAggregate  `json:"aggregate"`
	PerQuestion   []scenarios.QuestionScore `json:"per_question"`
	Latency       map[string]latencySummary `json:"latency"`
	StartedAt     time.Time                 `json:"started_at"`
	FinishedAt    time.Time                 `json:"finished_at"`
}

type budget struct {
	MaxItems  int `json:"max_items"`
	MaxTokens int `json:"max_tokens"`
}

type latencySummary struct {
	Calls   int   `json:"calls"`
	TotalMs int64 `json:"total_ms"`
}

func writeReport(path string, report *Report) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	defer func() { _ = file.Close() }()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func buildReport(
	opts runOptions,
	started time.Time,
	scores []scenarios.QuestionScore,
	latency *latencyAggregator,
	policyDigest string,
) *Report {
	return &Report{
		Suite:         opts.Scenario.Name(),
		Dataset:       opts.Dataset.Name,
		Host:          hostname(),
		Profile:       "sdk-default",
		PolicyDigest:  policyDigest,
		GenerateModel: opts.GenerateModel,
		EmbedModel:    opts.EmbedModel,
		AnswerModel:   opts.AnswerModel,
		JudgeModel:    opts.JudgeModel,
		Budget:        budget{MaxItems: opts.MaxItems, MaxTokens: opts.MaxTokens},
		N:             len(scores),
		Aggregate:     opts.Scenario.Aggregate(scores),
		PerQuestion:   append([]scenarios.QuestionScore(nil), scores...),
		Latency:       latency.snapshot(),
		StartedAt:     started,
		FinishedAt:    time.Now().UTC(),
	}
}

func hostname() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

func reportSummary(report *Report) string {
	if report == nil {
		return "report missing"
	}
	errors := 0
	for _, score := range report.PerQuestion {
		if score.Error != "" {
			errors++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "n=%d errors=%d qa.judge=%v qa.f1=%v qa.em=%v qa.item_recall=%v",
		report.N, errors, report.Aggregate.Judge, report.Aggregate.F1, report.Aggregate.EM, report.Aggregate.ItemRecall)
	if report.Aggregate.KHit != nil {
		fmt.Fprintf(&b, " recall.k_hit=%v", *report.Aggregate.KHit)
	}
	if report.Aggregate.Abstain != nil {
		fmt.Fprintf(&b, " qa.abstain=%v", *report.Aggregate.Abstain)
	}
	return b.String()
}
