package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// trackedRunner records how many questions were in flight at once and returns
// one item per question so the answers stay distinguishable.
type trackedRunner struct {
	inFlight atomic.Int64
	peak     atomic.Int64
	delay    time.Duration
}

func (runner *trackedRunner) CommitTurn(context.Context, corememory.Turn) error { return nil }
func (runner *trackedRunner) RunOnce(context.Context) error                     { return nil }

func (runner *trackedRunner) Context(_ context.Context, request corememory.ContextRequest) (corememory.ContextResult, error) {
	current := runner.inFlight.Add(1)
	defer runner.inFlight.Add(-1)
	for {
		peak := runner.peak.Load()
		if current <= peak || runner.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	if runner.delay > 0 {
		time.Sleep(runner.delay)
	}
	return corememory.ContextResult{Items: []corememory.ContextItem{{
		ID: "item-" + request.Query, Kind: corememory.ContextRawMessage,
		SourceClass: corememory.ContextSourceRecent,
		Content:     coremessage.NewTextContent("answer for " + request.Query),
	}}}, nil
}

func concurrencyScenario(count int) Scenario {
	questions := make([]Question, 0, count)
	for index := 0; index < count; index++ {
		questions = append(questions, Question{
			Query: fmt.Sprintf("q%02d", index), Category: 1 + index%4,
			WantContains: []string{fmt.Sprintf("q%02d", index)},
		})
	}
	return Scenario{
		Name: "concurrency", Scope: Scope{RuntimeID: "memories"}, ConversationID: "conv-1",
		Questions: questions,
	}
}

// echoAnswerer answers with the question id so a reordered report is visible.
type echoAnswerer struct{}

func (echoAnswerer) Answer(_ context.Context, question Question, _ []corememory.ContextItem) (string, error) {
	return "answer for " + question.Query, nil
}

// containsJudge accepts when the answer mentions the question's expectation.
type containsJudge struct{}

func (containsJudge) Judge(_ context.Context, question Question, answer string) (bool, error) {
	return containsAll(answer, question.WantContains), nil
}

func normalise(report Report) Report {
	report.MeanLatency = 0
	for index := range report.Questions {
		report.Questions[index].Latency = 0
	}
	return report
}

// TestConcurrentQuestionsMatchSequentialReport is the correctness contract for
// parallelism: same protocol, same order, same numbers. Concurrency may only
// shorten the wall clock.
func TestConcurrentQuestionsMatchSequentialReport(t *testing.T) {
	scenario := concurrencyScenario(24)
	options := Options{Answerer: echoAnswerer{}, Judge: containsJudge{}}

	sequential := &trackedRunner{}
	sequentialReport, err := RunWithOptions(context.Background(), sequential, scenario, options)
	if err != nil {
		t.Fatal(err)
	}

	concurrent := &trackedRunner{delay: 5 * time.Millisecond}
	parallelOptions := options
	parallelOptions.Concurrency = 4
	concurrentReport, err := RunWithOptions(context.Background(), concurrent, scenario, parallelOptions)
	if err != nil {
		t.Fatal(err)
	}

	if peak := concurrent.peak.Load(); peak < 2 || peak > 4 {
		t.Fatalf("in-flight questions peaked at %d, want 2..4", peak)
	}
	if !reflect.DeepEqual(normalise(sequentialReport), normalise(concurrentReport)) {
		first, _ := json.Marshal(normalise(sequentialReport))
		second, _ := json.Marshal(normalise(concurrentReport))
		t.Fatalf("reports differ:\n sequential: %s\n concurrent: %s", first, second)
	}
	for index, question := range concurrentReport.Questions {
		if want := fmt.Sprintf("q%02d", index); question.Query != want {
			t.Fatalf("question %d = %q, want %q: parallel run reordered results", index, question.Query, want)
		}
	}
}

// TestConcurrentQuestionsReportErrorsInOrder keeps failure behaviour stable: a
// parallel run reports the first failing question by index, not by whichever
// goroutine lost the race.
func TestConcurrentQuestionsReportErrorsInOrder(t *testing.T) {
	scenario := concurrencyScenario(8)
	report, err := RunWithOptions(context.Background(), &failingRunner{failOn: "q03"}, scenario,
		Options{Concurrency: 4})
	if err == nil {
		t.Fatal("parallel run swallowed the retrieval error")
	}
	if !strings.Contains(err.Error(), `question "q03"`) {
		t.Fatalf("error = %v, want the first failing question", err)
	}
	if report.Questions != nil {
		t.Fatalf("failed run returned a partial report: %#v", report.Questions)
	}
}

type failingRunner struct {
	failOn string
}

func (runner *failingRunner) CommitTurn(context.Context, corememory.Turn) error { return nil }
func (runner *failingRunner) RunOnce(context.Context) error                     { return nil }

func (runner *failingRunner) Context(_ context.Context, request corememory.ContextRequest) (corememory.ContextResult, error) {
	if request.Query == runner.failOn {
		return corememory.ContextResult{}, fmt.Errorf("boom")
	}
	return corememory.ContextResult{}, nil
}
