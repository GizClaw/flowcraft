package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	evalanswer "github.com/GizClaw/flowcraft/backends/memory/eval/answer"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// rejudgedQuestion is one stored answer with both verdicts.
type rejudgedQuestion struct {
	Scenario  string `json:"scenario"`
	Category  int    `json:"category"`
	Query     string `json:"query"`
	FullEvi   bool   `json:"full_evidence"`
	StrictHit bool   `json:"strict_hit"`
	LenientOK bool   `json:"lenient_hit"`
	Ungraded  bool   `json:"lenient_ungraded,omitempty"`
}

// TestRejudgeStoredAnswersWithLenientJudge re-grades the answers of a finished
// run with the leaderboard-style judge. Regenerating answers is not needed --
// the report stores them -- so this separates "the model answered wrong" from
// "the strict judge rejected a defensible answer" without paying for another
// answering pass.
//
// Input:  $MEMORY_EVAL_REPORT (default /tmp/locomo10-full.json)
// Output: $MEMORY_EVAL_REJUDGE (default /tmp/locomo10-rejudged.json)
func TestRejudgeStoredAnswersWithLenientJudge(t *testing.T) {
	envPath := liveEnvFile(t)
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	loadEnvFile(envPath)
	if os.Getenv("DEEPSEEK_API_KEY") == "" {
		t.Skip("DEEPSEEK_API_KEY is required")
	}
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to re-judge live")
	}
	reportPath := os.Getenv("MEMORY_EVAL_REPORT")
	if reportPath == "" {
		reportPath = "/tmp/locomo10-full.json"
	}
	outPath := os.Getenv("MEMORY_EVAL_REJUDGE")
	if outPath == "" {
		outPath = "/tmp/locomo10-rejudged.json"
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var baseline eval.Baseline
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	// The report stores the answers but not the gold, so re-join it with the
	// dataset: Answer walks a scenario's questions in order, so the stored
	// questions line up index by index.
	datasetRaw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(datasetRaw, eval.LoaderOptions{Scope: eval.Scope{RuntimeID: "memories"}})
	if err != nil {
		t.Fatal(err)
	}
	gold := make(map[string][]eval.Question, len(scenarios))
	for _, scenario := range scenarios {
		gold[scenario.Name] = scenario.Questions
	}

	built := buildAssembly(filepath.Join(workdir, "..", "..", "deploy.yaml"), 8)
	defer built.Close()
	// The judge model must be one the deploy actually declares: asking for a
	// name the endpoint does not serve now fails as unknown_model instead of
	// being silently substituted.
	newJudge := func(style evalanswer.JudgeStyle) *evalanswer.Model {
		judge, judgeErr := evalanswer.New(built.Inference, model.ModelRef{
			ID: model.ModelID{Provider: "deepseek", Name: "deepseek-flash"},
		})
		if judgeErr != nil {
			t.Fatal(judgeErr)
		}
		return judge.WithJudgeStyle(style)
	}
	strictJudge := newJudge(evalanswer.JudgeStrict)
	lenientJudge := newJudge(evalanswer.JudgeLocoMo)

	type job struct {
		scenario string
		question eval.Question
		answer   string
		fullEvi  bool
		strict   bool
		lenient  bool
		// lenientStored marks a job whose stored report carries a lenient
		// verdict: without it the judge-v1 line would report a rate over
		// answers that were never graded leniently as if they had failed.
		lenientStored bool
		strictV2      bool
		lenientV2     bool
		ungraded      bool
		lastErr       string
	}
	jobs := make([]job, 0, 1600)
	for _, report := range baseline.Reports {
		source, ok := gold[report.Scenario]
		if !ok {
			t.Fatalf("report scenario %q is not in the dataset", report.Scenario)
		}
		if len(source) != len(report.Questions) {
			t.Fatalf("scenario %s: report has %d questions, dataset %d",
				report.Scenario, len(report.Questions), len(source))
		}
		for index, question := range report.Questions {
			if source[index].Query != question.Query {
				t.Fatalf("scenario %s question %d: report %q, dataset %q",
					report.Scenario, index, question.Query, source[index].Query)
			}
			if question.Answer == "" || !question.AnswerGraded {
				continue
			}
			jobs = append(jobs, job{
				scenario: report.Scenario, question: eval.Question{
					Query: question.Query, Category: question.Category,
					WantContains: source[index].WantContains,
				},
				answer: question.Answer, fullEvi: question.Hit, strict: question.AnswerHit,
				lenient: question.LenientHit, lenientStored: question.LenientGraded,
			})
		}
	}
	if len(jobs) == 0 {
		t.Fatal("no stored answers to re-judge")
	}
	t.Logf("re-judging %d stored answers with both judge prompts (judge-v2)", len(jobs))

	const workers = 8
	var next atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(jobs) {
					return
				}
				current := &jobs[index]
				for _, target := range []struct {
					judge  *evalanswer.Model
					record *bool
				}{
					{strictJudge, &current.strictV2},
					{lenientJudge, &current.lenientV2},
				} {
					for attempt := 1; attempt <= 3; attempt++ {
						verdict, judgeErr := target.judge.Judge(context.Background(), current.question, current.answer)
						if judgeErr == nil {
							*target.record = verdict
							break
						}
						if attempt == 3 {
							current.ungraded = true
							current.lastErr = judgeErr.Error()
						}
					}
				}
			}
		}()
	}
	group.Wait()

	rows := make([]rejudgedQuestion, 0, len(jobs))
	var strictHits, lenientHits, lenientStored, strictV2Hits, lenientV2Hits, ungraded int
	for _, current := range jobs {
		if current.strict {
			strictHits++
		}
		if current.lenientStored {
			lenientStored++
			if current.lenient {
				lenientHits++
			}
		}
		if current.lenientV2 {
			lenientV2Hits++
		}
		if current.strictV2 {
			strictV2Hits++
		}
		if current.ungraded {
			ungraded++
		}
		rows = append(rows, rejudgedQuestion{
			Scenario: current.scenario, Category: current.question.Category, Query: current.question.Query,
			FullEvi: current.fullEvi, StrictHit: current.strictV2, LenientOK: current.lenientV2, Ungraded: current.ungraded,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Scenario != rows[j].Scenario {
			return rows[i].Scenario < rows[j].Scenario
		}
		return rows[i].Query < rows[j].Query
	})
	if err := writeRejudged(outPath, rows); err != nil {
		t.Fatal(err)
	}
	t.Logf("judge-v1 (stored): strict=%d/%d (%.4f)",
		strictHits, len(rows), float64(strictHits)/float64(len(rows)))
	if lenientStored == 0 {
		t.Logf("judge-v1 (stored): lenient=n/a (the stored report carries no lenient verdicts)")
	} else {
		t.Logf("judge-v1 (stored): lenient=%d/%d (%.4f)",
			lenientHits, lenientStored, float64(lenientHits)/float64(lenientStored))
	}
	t.Logf("judge-v2:          strict=%d/%d (%.4f) lenient=%d/%d (%.4f) ungraded=%d",
		strictV2Hits, len(rows), float64(strictV2Hits)/float64(len(rows)),
		lenientV2Hits, len(rows), float64(lenientV2Hits)/float64(len(rows)), ungraded)
	if ungraded > 0 {
		for _, current := range jobs {
			if current.ungraded && current.lastErr != "" {
				t.Logf("first ungraded error: %s", current.lastErr)
				break
			}
		}
	}
	t.Logf("wrote %s", outPath)
}

func writeRejudged(path string, rows []rejudgedQuestion) error {
	encoded, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}
