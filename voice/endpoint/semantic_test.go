package endpoint

import (
	"testing"
	"time"
)

func TestSemanticDecider_FullSilenceWithoutSemantic(t *testing.T) {
	d := NewSemanticSilenceDecider(500*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true})
	for i := 0; i < 4; i++ {
		if got := d.Feed(Input{}); got != Continue {
			t.Fatalf("frame %d: got %v, want Continue", i, got)
		}
	}
	if got := d.Feed(Input{}); got != Commit {
		t.Fatalf("frame 5: got %v, want Commit (full limit = 5 frames)", got)
	}
}

func TestSemanticDecider_ReducedSilenceWithTerminal(t *testing.T) {
	// fullLimit = 500ms/100ms = 5, reducedLimit = 200ms/100ms = 2
	d := NewSemanticSilenceDecider(500*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true, PartialText: "你好吗？"})
	if got := d.Feed(Input{PartialText: "你好吗？"}); got != Continue {
		t.Fatalf("first silent frame: got %v, want Continue", got)
	}
	if got := d.Feed(Input{PartialText: "你好吗？"}); got != Commit {
		t.Fatalf("second silent frame with terminal: got %v, want Commit (reduced limit = 2)", got)
	}
}

func TestSemanticDecider_TerminalDisappearsReverts(t *testing.T) {
	d := NewSemanticSilenceDecider(500*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true, PartialText: "你好吗？"})
	// One silent frame with terminal
	d.Feed(Input{PartialText: "你好吗？"})
	// Partial updates to remove terminal — falls back to full limit
	if got := d.Feed(Input{PartialText: "你好吗？我想"}); got != Continue {
		t.Fatalf("got %v, want Continue (terminal disappeared, need full limit)", got)
	}
}

func TestSemanticDecider_SpeechResetsCounter(t *testing.T) {
	d := NewSemanticSilenceDecider(500*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true, PartialText: "你好？"})
	d.Feed(Input{PartialText: "你好？"}) // silentN=1
	// User starts speaking again
	d.Feed(Input{IsSpeech: true, PartialText: "你好？我想"})
	// Counter should have been reset; need full reducedLimit again
	if got := d.Feed(Input{PartialText: "你好？我想问"}); got != Continue {
		t.Fatalf("got %v, want Continue (counter was reset by speech)", got)
	}
}

func TestSemanticDecider_ExplicitCommit(t *testing.T) {
	d := NewSemanticSilenceDecider(500*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true})
	if got := d.Feed(Input{ExplicitCommit: true}); got != Commit {
		t.Fatalf("explicit commit: got %v, want Commit", got)
	}
}

func TestSemanticDecider_CustomTerminals(t *testing.T) {
	d := NewSemanticSilenceDecider(
		500*time.Millisecond, 100*time.Millisecond,
		WithTerminals("。？"),
	)

	d.Feed(Input{IsSpeech: true})
	// Exclamation mark is NOT a terminal in this config
	d.Feed(Input{PartialText: "太好了！"})
	if got := d.Feed(Input{PartialText: "太好了！"}); got != Continue {
		t.Fatalf("got %v, want Continue (! is not a terminal)", got)
	}
}

func TestSemanticDecider_CustomReducedSilence(t *testing.T) {
	// reducedLimit = 300ms/100ms = 3
	d := NewSemanticSilenceDecider(
		500*time.Millisecond, 100*time.Millisecond,
		WithReducedSilence(300*time.Millisecond),
	)

	d.Feed(Input{IsSpeech: true, PartialText: "好的。"})
	d.Feed(Input{PartialText: "好的。"}) // silentN=1
	if got := d.Feed(Input{PartialText: "好的。"}); got != Continue {
		t.Fatalf("frame 2: got %v, want Continue (reduced limit = 3)", got)
	}
	if got := d.Feed(Input{PartialText: "好的。"}); got != Commit {
		t.Fatalf("frame 3: got %v, want Commit", got)
	}
}

func TestSemanticDecider_ReducedLimitCapped(t *testing.T) {
	// reducedSilence > silenceDuration → should be capped
	d := NewSemanticSilenceDecider(
		200*time.Millisecond, 100*time.Millisecond,
		WithReducedSilence(500*time.Millisecond),
	)

	d.Feed(Input{IsSpeech: true, PartialText: "好。"})
	if got := d.Feed(Input{PartialText: "好。"}); got != Continue {
		t.Fatalf("frame 1: got %v, want Continue", got)
	}
	if got := d.Feed(Input{PartialText: "好。"}); got != Commit {
		t.Fatalf("frame 2: got %v, want Commit (capped to fullLimit=2)", got)
	}
}

func TestSemanticDecider_EmptyPartialNoEffect(t *testing.T) {
	d := NewSemanticSilenceDecider(500*time.Millisecond, 100*time.Millisecond)

	d.Feed(Input{IsSpeech: true})
	for i := 0; i < 4; i++ {
		if got := d.Feed(Input{PartialText: ""}); got != Continue {
			t.Fatalf("frame %d: got %v, want Continue", i, got)
		}
	}
	if got := d.Feed(Input{PartialText: ""}); got != Commit {
		t.Fatalf("should commit at full limit when partial is empty")
	}
}
