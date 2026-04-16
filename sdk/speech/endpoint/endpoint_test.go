package endpoint

import (
	"testing"
	"time"
)

func TestSilenceDecider_CommitsAfterSilenceLimit(t *testing.T) {
	d := NewSilenceDecider(300*time.Millisecond, 100*time.Millisecond)

	if got := d.Feed(Input{IsSpeech: true}); got != Continue {
		t.Fatalf("speech frame = %v, want Continue", got)
	}
	if got := d.Feed(Input{IsSpeech: false}); got != Continue {
		t.Fatalf("first silence frame = %v, want Continue", got)
	}
	if got := d.Feed(Input{IsSpeech: false}); got != Continue {
		t.Fatalf("second silence frame = %v, want Continue", got)
	}
	if got := d.Feed(Input{IsSpeech: false}); got != Commit {
		t.Fatalf("third silence frame = %v, want Commit", got)
	}
}

func TestSilenceDecider_ExplicitCommit(t *testing.T) {
	d := NewSilenceDecider(700*time.Millisecond, 100*time.Millisecond)

	if got := d.Feed(Input{IsSpeech: true}); got != Continue {
		t.Fatalf("speech frame = %v, want Continue", got)
	}
	if got := d.Feed(Input{ExplicitCommit: true}); got != Commit {
		t.Fatalf("explicit commit = %v, want Commit", got)
	}
	if got := d.Feed(Input{IsSpeech: false}); got != Continue {
		t.Fatalf("state should reset after explicit commit, got %v", got)
	}
}

func TestSilenceDecider_CeilingDivision(t *testing.T) {
	// 150ms / 100ms should give limit = ceil(1.5) = 2, not floor(1.5) = 1.
	d := NewSilenceDecider(150*time.Millisecond, 100*time.Millisecond)

	if got := d.Feed(Input{IsSpeech: true}); got != Continue {
		t.Fatalf("speech frame = %v, want Continue", got)
	}
	if got := d.Feed(Input{IsSpeech: false}); got != Continue {
		t.Fatalf("first silence = %v, want Continue (limit should be 2)", got)
	}
	if got := d.Feed(Input{IsSpeech: false}); got != Commit {
		t.Fatalf("second silence = %v, want Commit", got)
	}
}

func TestSilenceDecider_SingleFrame(t *testing.T) {
	// 50ms / 100ms => ceil(0.5)=1, should commit after 1 silence frame.
	d := NewSilenceDecider(50*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true})
	if got := d.Feed(Input{IsSpeech: false}); got != Commit {
		t.Fatalf("should commit after 1 frame, got %v", got)
	}
}

func TestFramesToLimit_ZeroDuration(t *testing.T) {
	got := framesToLimit(0, 100*time.Millisecond)
	if got != 1 {
		t.Fatalf("framesToLimit(0, 100ms) = %d, want 1", got)
	}
}

func TestFramesToLimit_ZeroFrameSize(t *testing.T) {
	got := framesToLimit(300*time.Millisecond, 0)
	if got != 1 {
		t.Fatalf("framesToLimit(300ms, 0) = %d, want 1", got)
	}
}
