package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Report is the JSON artifact of one eval run.
type Report struct {
	Suite         string                    `json:"suite"`
	Dataset       string                    `json:"dataset"`
	Profile       string                    `json:"profile"`
	PolicyDigest  string                    `json:"policy_digest"`
	GenerateModel string                    `json:"generate_model"`
	EmbedModel    string                    `json:"embed_model"`
	AnswerModel   string                    `json:"answer_model"`
	JudgeModel    string                    `json:"judge_model,omitempty"`
	Budget        budget                    `json:"budget"`
	N             int                       `json:"n"`
	Aggregate     scoreAggregate            `json:"aggregate"`
	PerQuestion   []questionScore           `json:"per_question"`
	Latency       map[string]latencySummary `json:"latency"`
	StartedAt     time.Time                 `json:"started_at"`
	FinishedAt    time.Time                 `json:"finished_at"`
}

type budget struct {
	MaxItems  int `json:"max_items"`
	MaxTokens int `json:"max_tokens"`
}

type scoreAggregate struct {
	EM          float64                  `json:"qa.em"`
	F1          float64                  `json:"qa.f1"`
	Judge       float64                  `json:"qa.judge"`
	KHit        *float64                 `json:"recall.k_hit,omitempty"`
	KHitMessage *float64                 `json:"recall.k_hit_message,omitempty"`
	KHitFact    *float64                 `json:"recall.k_hit_fact,omitempty"`
	ByCategory  map[string]categoryScore `json:"by_category,omitempty"`
}

type categoryScore struct {
	Count int      `json:"count"`
	EM    float64  `json:"qa.em"`
	F1    float64  `json:"qa.f1"`
	Judge float64  `json:"qa.judge"`
	KHit  *float64 `json:"recall.k_hit,omitempty"`
}

type questionScore struct {
	ID            string   `json:"id"`
	Query         string   `json:"query"`
	Prediction    string   `json:"prediction"`
	EM            float64  `json:"em"`
	F1            float64  `json:"f1"`
	Judge         float64  `json:"judge,omitempty"`
	JudgeScored   bool     `json:"-"`
	KHit          *float64 `json:"k_hit,omitempty"`
	KHitMessage   *float64 `json:"k_hit_message,omitempty"`
	KHitFact      *float64 `json:"k_hit_fact,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	ItemCount     int      `json:"item_count"`
	EvidenceCount int      `json:"evidence_count"`
	Error         string   `json:"error,omitempty"`
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
	defer file.Close()
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
	scores []questionScore,
	latency *latencyAggregator,
	policyDigest string,
) *Report {
	aggregate := aggregateScores(scores)
	return &Report{
		Suite:         opts.Suite,
		Dataset:       opts.Dataset.Name,
		Profile:       "sdk-default",
		PolicyDigest:  policyDigest,
		GenerateModel: opts.GenerateModel,
		EmbedModel:    opts.EmbedModel,
		AnswerModel:   opts.AnswerModel,
		JudgeModel:    opts.JudgeModel,
		Budget:        budget{MaxItems: opts.MaxItems, MaxTokens: opts.MaxTokens},
		N:             len(scores),
		Aggregate:     aggregate,
		PerQuestion:   append([]questionScore(nil), scores...),
		Latency:       latency.snapshot(),
		StartedAt:     started,
		FinishedAt:    time.Now().UTC(),
	}
}

func aggregateScores(scores []questionScore) scoreAggregate {
	var aggregate scoreAggregate
	if len(scores) == 0 {
		return aggregate
	}
	judgeCount := 0
	var (
		kHitSum, kHitMessageSum, kHitFactSum       float64
		kHitCount, kHitMessageCount, kHitFactCount int
	)
	for _, score := range scores {
		aggregate.EM += score.EM
		aggregate.F1 += score.F1
		if score.JudgeScored {
			aggregate.Judge += score.Judge
			judgeCount++
		}
		if score.KHit != nil {
			kHitSum += *score.KHit
			kHitCount++
		}
		if score.KHitMessage != nil {
			kHitMessageSum += *score.KHitMessage
			kHitMessageCount++
		}
		if score.KHitFact != nil {
			kHitFactSum += *score.KHitFact
			kHitFactCount++
		}
	}
	aggregate.EM /= float64(len(scores))
	aggregate.F1 /= float64(len(scores))
	if judgeCount > 0 {
		aggregate.Judge /= float64(judgeCount)
	}
	aggregate.KHit = meanPtr(kHitSum, kHitCount)
	aggregate.KHitMessage = meanPtr(kHitMessageSum, kHitMessageCount)
	aggregate.KHitFact = meanPtr(kHitFactSum, kHitFactCount)
	aggregate.ByCategory = aggregateByCategory(scores)
	return aggregate
}

func aggregateByCategory(scores []questionScore) map[string]categoryScore {
	type accumulator struct {
		count                       int
		em, f1, judge               float64
		judgeCount                  int
		kHitSum, kHitMessageSum     float64
		kHitCount, kHitMessageCount int
	}
	groups := make(map[string]*accumulator)
	for _, score := range scores {
		for _, tag := range score.Tags {
			if categoryKey(tag) == "" {
				continue
			}
			group := groups[tag]
			if group == nil {
				group = &accumulator{}
				groups[tag] = group
			}
			group.count++
			group.em += score.EM
			group.f1 += score.F1
			if score.JudgeScored {
				group.judge += score.Judge
				group.judgeCount++
			}
			if score.KHit != nil {
				group.kHitSum += *score.KHit
				group.kHitCount++
			}
		}
	}
	if len(groups) == 0 {
		return nil
	}
	result := make(map[string]categoryScore, len(groups))
	for tag, group := range groups {
		category := categoryScore{
			Count: group.count,
			EM:    group.em / float64(group.count),
			F1:    group.f1 / float64(group.count),
		}
		if group.judgeCount > 0 {
			category.Judge = group.judge / float64(group.judgeCount)
		}
		category.KHit = meanPtr(group.kHitSum, group.kHitCount)
		result[tag] = category
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]categoryScore, len(keys))
	for _, key := range keys {
		ordered[key] = result[key]
	}
	return ordered
}

func categoryKey(tag string) string {
	if len(tag) > 3 && strings.HasPrefix(tag, "cat") && allDigits(tag[3:]) {
		return ""
	}
	return tag
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func meanPtr(sum float64, count int) *float64 {
	if count == 0 {
		return nil
	}
	value := sum / float64(count)
	return &value
}
