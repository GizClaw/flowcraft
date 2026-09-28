package official

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
)

// TestScoreStoredReport re-scores a finished report with the leaderboard metric.
// The answers are already on disk, so this costs no model calls and needs no
// credentials: it is the "same ruler" number we can put next to published
// LoCoMo results.
func TestScoreStoredReport(t *testing.T) {
	reportPath := os.Getenv("MEMORY_EVAL_REPORT")
	if reportPath == "" {
		t.Skip("set MEMORY_EVAL_REPORT to a report written by cmd/memory-eval")
	}
	raw, err := readModuleFile(reportPath)
	if err != nil {
		t.Fatalf("MEMORY_EVAL_REPORT=%q: %v", reportPath, err)
	}
	var baseline eval.Baseline
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	datasetPath := os.Getenv("MEMORY_EVAL_DATASET")
	if datasetPath == "" {
		datasetPath = filepath.Join("..", "locomo10.json")
	}
	dataset, err := readModuleFile(datasetPath)
	if err != nil {
		t.Fatalf("MEMORY_EVAL_DATASET=%q: %v", datasetPath, err)
	}
	scenarios, _, err := eval.LoadLoCoMo(dataset, eval.LoaderOptions{
		Scope: eval.Scope{RuntimeID: "memories"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gold := make(map[string]eval.Question, len(scenarios))
	for _, scenario := range scenarios {
		for _, question := range scenario.Questions {
			gold[scenario.Name+"\x00"+question.Query] = question
		}
	}

	type aggregate struct {
		Questions int
		Score     float64
	}
	byCategory := map[int]*aggregate{}
	overall := &aggregate{}
	for _, report := range baseline.Reports {
		for _, result := range report.Questions {
			question, ok := gold[report.Scenario+"\x00"+result.Query]
			if !ok || result.Answer == "" {
				continue
			}
			score := Score(result.Category, result.Answer, question.WantContains)
			entry := byCategory[result.Category]
			if entry == nil {
				entry = &aggregate{}
				byCategory[result.Category] = entry
			}
			entry.Questions++
			entry.Score += score
			overall.Questions++
			overall.Score += score
		}
	}
	categories := make([]int, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, category)
	}
	sort.Ints(categories)
	t.Logf("official token-F1 over %s (categories 1-4; category 5 is excluded by the harness)", filepath.Base(reportPath))
	for _, category := range categories {
		entry := byCategory[category]
		t.Logf("  category %d: questions=%4d f1=%.4f", category, entry.Questions, entry.Score/float64(entry.Questions))
	}
	t.Logf("  overall   : questions=%4d f1=%.4f", overall.Questions, overall.Score/float64(overall.Questions))
}

// readModuleFile reads a file the way the harness names it: relative to the eval
// module (backends/memory/eval), not to this package's working directory. A
// report lives beside the module, so `MEMORY_EVAL_REPORT=report-x.json` means
// `../report-x.json` from here -- reading it verbatim failed with "no such
// file", which reads like a missing report rather than a resolved path.
func readModuleFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		return raw, nil
	}
	return os.ReadFile(filepath.Join("..", path))
}
