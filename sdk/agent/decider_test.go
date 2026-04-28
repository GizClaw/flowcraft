package agent

import (
	"context"
	"errors"
	"testing"
)

// Internal-package tests for runDeciders / FinalizeDecision merging.
// runDeciders is unexported so these stay in package agent.

type stubDecider struct {
	dec FinalizeDecision
	err error
}

func (s stubDecider) BeforeFinalize(context.Context, RunInfo, *Request, *Result) (FinalizeDecision, error) {
	return s.dec, s.err
}

func TestFinalizeDecision_Merge_BoolsORed(t *testing.T) {
	a := FinalizeDecision{DiscardOutput: true}
	b := FinalizeDecision{Revise: true}

	got := a.merge(b)
	if !got.DiscardOutput || !got.Revise {
		t.Errorf("merge OR over bools failed: %+v", got)
	}
}

func TestFinalizeDecision_Merge_FirstNonEmptyReasonWins(t *testing.T) {
	first := FinalizeDecision{Reason: "first"}
	second := FinalizeDecision{Reason: "second"}

	got := first.merge(second)
	if got.Reason != "first" {
		t.Errorf("Reason = %q, want %q", got.Reason, "first")
	}

	got2 := FinalizeDecision{}.merge(second)
	if got2.Reason != "second" {
		t.Errorf("merge into empty Reason = %q, want %q", got2.Reason, "second")
	}
}

func TestRunDeciders_NilEntriesSkipped(t *testing.T) {
	got, err := runDeciders(context.Background(),
		[]Decider{nil, stubDecider{dec: FinalizeDecision{Reason: "ok"}}, nil},
		RunInfo{}, &Request{}, &Result{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Reason != "ok" {
		t.Errorf("Reason = %q, want %q", got.Reason, "ok")
	}
}

func TestRunDeciders_FirstErrorShortCircuits(t *testing.T) {
	boom := errors.New("decider boom")
	called := 0
	d2 := stubFn(func() (FinalizeDecision, error) {
		called++
		return FinalizeDecision{Reason: "should-not-merge"}, nil
	})

	_, err := runDeciders(context.Background(),
		[]Decider{stubDecider{err: boom}, d2},
		RunInfo{}, &Request{}, &Result{})
	if !errors.Is(err, boom) {
		t.Errorf("expected boom; got %v", err)
	}
	if called != 0 {
		t.Errorf("subsequent deciders ran after error; called=%d", called)
	}
}

func TestRunDeciders_AccumulatesAcrossDeciders(t *testing.T) {
	got, err := runDeciders(context.Background(),
		[]Decider{
			stubDecider{dec: FinalizeDecision{Reason: "a"}},
			stubDecider{dec: FinalizeDecision{DiscardOutput: true}},
			stubDecider{dec: FinalizeDecision{Revise: true}},
		},
		RunInfo{}, &Request{}, &Result{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.DiscardOutput || !got.Revise {
		t.Errorf("OR fold lost a bool: %+v", got)
	}
	if got.Reason != "a" {
		t.Errorf("first non-empty Reason should win: %q", got.Reason)
	}
}

func TestBaseDecider_ZeroValueDecision(t *testing.T) {
	dec, err := BaseDecider{}.BeforeFinalize(context.Background(), RunInfo{}, &Request{}, &Result{})
	if err != nil {
		t.Errorf("BaseDecider returned error: %v", err)
	}
	if (dec != FinalizeDecision{}) {
		t.Errorf("BaseDecider returned non-zero decision: %+v", dec)
	}
}

type stubFn func() (FinalizeDecision, error)

func (f stubFn) BeforeFinalize(context.Context, RunInfo, *Request, *Result) (FinalizeDecision, error) {
	return f()
}
