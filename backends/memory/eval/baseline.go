package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Baseline records the hit rate of every scenario from one run.
type Baseline struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Fingerprint records the configuration the reports were measured under.
	// Comparing a baseline whose fingerprint differs is comparing two different
	// experiments, so callers warn before treating a delta as a regression.
	Fingerprint Fingerprint `json:"fingerprint,omitempty"`
	Reports     []Report    `json:"reports"`
}

// NewBaseline captures the current reports.
func NewBaseline(name string, reports []Report) Baseline {
	return Baseline{
		Name: name, CreatedAt: time.Now().UTC(),
		Reports: append([]Report(nil), reports...),
	}
}

// Regression is one scenario whose hit rate dropped beyond tolerance.
type Regression struct {
	Scenario string  `json:"scenario"`
	Metric   string  `json:"metric,omitempty"`
	Baseline float64 `json:"baseline_hit_rate"`
	Current  float64 `json:"current_hit_rate"`
	Delta    float64 `json:"delta"`
}

// Compare reports scenarios whose hit rate fell by more than tolerance.
// Scenarios missing from either side are ignored.
func (baseline Baseline) Compare(current []Report, tolerance float64) []Regression {
	index := make(map[string]float64, len(baseline.Reports))
	for _, report := range baseline.Reports {
		index[report.Scenario] = report.HitRate
	}
	var regressions []Regression
	for _, report := range current {
		previous, ok := index[report.Scenario]
		if !ok {
			continue
		}
		delta := report.HitRate - previous
		if delta < -tolerance {
			regressions = append(regressions, Regression{
				Scenario: report.Scenario, Metric: "hit_rate",
				Baseline: previous, Current: report.HitRate, Delta: delta,
			})
		}
	}
	return regressions
}

// CompareAnswers reports scenarios whose generative answer rate fell by more
// than tolerance. Scenarios that carry no answer rate on either side are
// ignored so recall-only runs stay comparable.
func (baseline Baseline) CompareAnswers(current []Report, tolerance float64) []Regression {
	index := make(map[string]float64, len(baseline.Reports))
	for _, report := range baseline.Reports {
		index[report.Scenario] = report.AnswerRate
	}
	var regressions []Regression
	for _, report := range current {
		previous, ok := index[report.Scenario]
		if !ok {
			continue
		}
		if previous == 0 && report.AnswerRate == 0 {
			continue
		}
		delta := report.AnswerRate - previous
		if delta < -tolerance {
			regressions = append(regressions, Regression{
				Scenario: report.Scenario, Metric: "answer_rate",
				Baseline: previous, Current: report.AnswerRate, Delta: delta,
			})
		}
	}
	return regressions
}

// SaveBaseline writes one strict JSON baseline document.
func SaveBaseline(writer io.Writer, baseline Baseline) error {
	encoded, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return fmt.Errorf("memory eval: encode baseline: %w", err)
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("memory eval: write baseline: %w", err)
	}
	return nil
}

// LoadBaseline reads one strict JSON baseline document.
func LoadBaseline(reader io.Reader) (Baseline, error) {
	var baseline Baseline
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&baseline); err != nil {
		return Baseline{}, fmt.Errorf("memory eval: decode baseline: %w", err)
	}
	return baseline, nil
}
