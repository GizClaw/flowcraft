package scenarios

import (
	"sort"
	"strings"
)

// ScoreAggregate is the headline metrics of one eval run.
type ScoreAggregate struct {
	EM          float64                  `json:"qa.em"`
	F1          float64                  `json:"qa.f1"`
	Judge       float64                  `json:"qa.judge"`
	Abstain     *float64                 `json:"qa.abstain,omitempty"`
	KHit        *float64                 `json:"recall.k_hit,omitempty"`
	KHitMessage *float64                 `json:"recall.k_hit_message,omitempty"`
	KHitFact    *float64                 `json:"recall.k_hit_fact,omitempty"`
	ByCategory  map[string]CategoryScore `json:"by_category,omitempty"`
}

// CategoryScore is one category's slice of the headline metrics.
type CategoryScore struct {
	Count int      `json:"count"`
	EM    float64  `json:"qa.em"`
	F1    float64  `json:"qa.f1"`
	Judge float64  `json:"qa.judge"`
	KHit  *float64 `json:"recall.k_hit,omitempty"`
}

// QuestionScore is one question's per-metric breakdown.
type QuestionScore struct {
	ID            string   `json:"id"`
	Query         string   `json:"query"`
	Prediction    string   `json:"prediction"`
	EM            float64  `json:"em"`
	F1            float64  `json:"f1"`
	Judge         float64  `json:"judge,omitempty"`
	JudgeScored   bool     `json:"-"`
	Abstention    *float64 `json:"abstention,omitempty"`
	KHit          *float64 `json:"k_hit,omitempty"`
	KHitMessage   *float64 `json:"k_hit_message,omitempty"`
	KHitFact      *float64 `json:"k_hit_fact,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	ItemCount     int      `json:"item_count"`
	EvidenceCount int      `json:"evidence_count"`
	Error         string   `json:"error,omitempty"`
}

func aggregateScores(scores []QuestionScore) ScoreAggregate {
	var aggregate ScoreAggregate
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

func aggregateByCategory(scores []QuestionScore) map[string]CategoryScore {
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
	result := make(map[string]CategoryScore, len(groups))
	for tag, group := range groups {
		category := CategoryScore{
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
	ordered := make(map[string]CategoryScore, len(keys))
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

func abstainMean(scores []QuestionScore) *float64 {
	sum := 0.0
	count := 0
	for _, score := range scores {
		if score.Abstention != nil {
			sum += *score.Abstention
			count++
		}
	}
	return meanPtr(sum, count)
}
