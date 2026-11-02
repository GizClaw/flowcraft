package maintain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// overlappingDecisions answers like the real surface and records how many
// calls were in flight at once, which is the only thing the schedule changes.
// A call holds for a moment so that two of them can overlap at all: without
// the wait a worker pool finishes each call before the next one starts and the
// test would pass on a serial loop.
type overlappingDecisions struct {
	delay time.Duration
	fail  func(call int) error

	mu       sync.Mutex
	calls    int
	inFlight int
	peak     int
}

func (decisions *overlappingDecisions) Decision(
	ctx context.Context,
	_ model.ModelRef,
	request inference.DecisionRequest,
) (inference.DecisionResponse, error) {
	decisions.mu.Lock()
	call := decisions.calls
	decisions.calls++
	decisions.inFlight++
	if decisions.inFlight > decisions.peak {
		decisions.peak = decisions.inFlight
	}
	decisions.mu.Unlock()
	defer func() {
		decisions.mu.Lock()
		decisions.inFlight--
		decisions.mu.Unlock()
	}()
	if decisions.fail != nil {
		if err := decisions.fail(call); err != nil {
			return inference.DecisionResponse{}, err
		}
	}
	select {
	case <-time.After(decisions.delay):
	case <-ctx.Done():
		return inference.DecisionResponse{}, ctx.Err()
	}
	response := inference.DecisionResponse{}
	for _, question := range request.Questions {
		// The answer reads the question, so a judgment that landed in the
		// wrong slot is visible: only "update" statements are replaced.
		verdict, probability := VerdictBothCurrent, 0.9
		if strings.Contains(question.Instructions, "update-") {
			verdict, probability = VerdictReplaced, 0.9
		}
		response.Answers = append(response.Answers, choiceAnswer(question.Name, string(verdict), probability))
	}
	return response, nil
}

func (decisions *overlappingDecisions) snapshot() (calls, peak int) {
	decisions.mu.Lock()
	defer decisions.mu.Unlock()
	return decisions.calls, decisions.peak
}

func overlappingArbiter(decisions *overlappingDecisions, concurrency int) *DecisionArbiter {
	return &DecisionArbiter{
		Decisions:   decisions,
		Model:       model.ModelRef{ID: model.ModelID{Provider: "deepseek", Name: "azure-decision"}},
		Batch:       1,
		Concurrency: concurrency,
	}
}

func mixedPairs(count int) []Pair {
	pairs := make([]Pair, count)
	for index := range pairs {
		statement := "keep-"
		if index%2 == 0 {
			statement = "update-"
		}
		pairs[index] = arbiterPair("older-"+statement+string(rune('a'+index)), "newer-"+statement+string(rune('a'+index)))
	}
	return pairs
}

// TestDecisionArbiterKeepsSeveralBatchesInFlight pins the schedule: the batches
// are independent questions, so a pass keeps several of them in the air
// instead of waiting out one round trip at a time.
func TestDecisionArbiterKeepsSeveralBatchesInFlight(t *testing.T) {
	decisions := &overlappingDecisions{delay: 15 * time.Millisecond}
	arbiter := overlappingArbiter(decisions, 4)
	judgments, err := arbiter.Adjudicate(context.Background(), mixedPairs(8))
	if err != nil {
		t.Fatal(err)
	}
	calls, peak := decisions.snapshot()
	if calls != 8 {
		t.Fatalf("calls = %d, want one per batch", calls)
	}
	if peak < 2 || peak > 4 {
		t.Fatalf("peak in flight = %d, want 2..4", peak)
	}
	for index, judgment := range judgments {
		want := VerdictBothCurrent
		if index%2 == 0 {
			want = VerdictReplaced
		}
		if judgment.Verdict != want {
			t.Fatalf("judgment %d = %s, want %s: the batch landed in the wrong slot", index, judgment.Verdict, want)
		}
	}
}

// TestDecisionArbiterConcurrencyBoundsTheSchedule pins both ends of the knob:
// one worker is serial, and a deployment cannot ask for more workers than the
// ceiling allows.
func TestDecisionArbiterConcurrencyBoundsTheSchedule(t *testing.T) {
	serial := &overlappingDecisions{delay: 5 * time.Millisecond}
	if _, err := overlappingArbiter(serial, 1).Adjudicate(context.Background(), mixedPairs(4)); err != nil {
		t.Fatal(err)
	}
	if _, peak := serial.snapshot(); peak != 1 {
		t.Fatalf("peak in flight = %d, want 1", peak)
	}
	capped := overlappingArbiter(&overlappingDecisions{}, maxArbiterConcurrency+8)
	if got := capped.concurrency(); got != maxArbiterConcurrency {
		t.Fatalf("concurrency = %d, want the ceiling %d", got, maxArbiterConcurrency)
	}
	if got := (&DecisionArbiter{}).concurrency(); got != defaultArbiterConcurrency {
		t.Fatalf("default concurrency = %d", got)
	}
	// The schedule is not part of the judgment: two arbiters that differ only
	// here serve the same overlay.
	stamped := &DecisionArbiter{Model: capped.Model, Concurrency: 1}
	if stamped.Identity() != capped.Identity() {
		t.Fatalf("concurrency entered the identity: %q vs %q", stamped.Identity(), capped.Identity())
	}
}

// TestDecisionArbiterFailsTheWholeAnswer pins what a caller reads: one failed
// batch means no judgments at all, because the pass publishes the arbiter's
// half whole or not at all. The batches still in flight are cancelled rather
// than waited out.
func TestDecisionArbiterFailsTheWholeAnswer(t *testing.T) {
	decisions := &overlappingDecisions{
		delay: 20 * time.Millisecond,
		fail: func(call int) error {
			if call == 0 {
				return errors.New("upstream_invalid_request")
			}
			return nil
		},
	}
	arbiter := overlappingArbiter(decisions, 2)
	judgments, err := arbiter.Adjudicate(context.Background(), mixedPairs(32))
	if err == nil {
		t.Fatalf("judgments = %#v", judgments)
	}
	if !strings.Contains(err.Error(), "upstream_invalid_request") {
		t.Fatalf("error = %v", err)
	}
	if judgments != nil {
		t.Fatalf("a failed answer returned %d judgments", len(judgments))
	}
	// The first failure cancels the workers, so a run of 32 batches does not
	// pay for 32 calls to learn the same thing.
	if calls, _ := decisions.snapshot(); calls == 32 {
		t.Fatalf("calls = %d: the failure did not stop the remaining batches", calls)
	}
}

// TestDecisionArbiterAnswersTheSameUnderAnySchedule pins determinism: the
// answers are indexed by the pair they answer, so the order the batches finish
// in changes nothing a caller can read.
func TestDecisionArbiterAnswersTheSameUnderAnySchedule(t *testing.T) {
	pairs := mixedPairs(12)
	serial, err := overlappingArbiter(&overlappingDecisions{}, 1).Adjudicate(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := overlappingArbiter(&overlappingDecisions{}, 4).Adjudicate(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	for index := range pairs {
		if serial[index] != parallel[index] {
			t.Fatalf("judgment %d = %+v serial, %+v parallel", index, serial[index], parallel[index])
		}
	}
}
