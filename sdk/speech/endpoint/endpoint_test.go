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
