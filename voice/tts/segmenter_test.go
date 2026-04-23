package tts

import "testing"

func TestSegmenter_DefaultTerminators(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1))
	sentence, ok := seg.Feed("你好。世界")
	if !ok {
		t.Fatal("expected break at Chinese period")
	}
	if sentence != "你好。" {
		t.Fatalf("sentence = %q, want %q", sentence, "你好。")
	}
}

func TestSegmenter_CustomTerminators(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1), WithTerminators("؟"))

	if _, ok := seg.Feed("هل أنت بخير"); ok {
		t.Fatal("should not break without custom terminator")
	}

	sentence, ok := seg.Feed("؟")
	if !ok {
		t.Fatal("expected break at Arabic question mark")
	}
	if sentence != "هل أنت بخير؟" {
		t.Fatalf("sentence = %q", sentence)
	}
}

func TestSegmenter_CustomTerminatorsOverridesDefault(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1), WithTerminators("@"))

	seg.Feed("hello.")
	remainder := seg.Flush()
	if remainder != "hello." {
		t.Fatalf("should not break at period with custom terminators, got flush = %q", remainder)
	}
}

func TestSegmenter_CustomWeakBreaks(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1), EagerMode(), WithWeakBreaks("→"))

	sentence, ok := seg.Feed("hello→world")
	if !ok {
		t.Fatal("expected break at custom weak break character")
	}
	if sentence != "hello→" {
		t.Fatalf("sentence = %q, want %q", sentence, "hello→")
	}
}

func TestSegmenter_DefaultWeakBreaksInEagerMode(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1), EagerMode())

	// Eager first-sentence requires min 4 runes before a weak break triggers.
	sentence, ok := seg.Feed("这是测试，后面")
	if !ok {
		t.Fatal("expected break at Chinese comma in eager mode")
	}
	if sentence != "这是测试，" {
		t.Fatalf("sentence = %q, want %q", sentence, "这是测试，")
	}
}

func TestSegmenter_Feed_MultipleSentences(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1))

	s1, ok := seg.Feed("Hello world.")
	if !ok {
		t.Fatal("expected first sentence")
	}
	if s1 != "Hello world." {
		t.Fatalf("s1 = %q, want %q", s1, "Hello world.")
	}

	s2, ok := seg.Feed(" Goodbye.")
	if !ok {
		t.Fatal("expected second sentence")
	}
	if s2 != "Goodbye." {
		t.Fatalf("s2 = %q, want %q", s2, "Goodbye.")
	}
}

func TestSegmenter_ForceBreak(t *testing.T) {
	seg := NewSegmenter(WithMinChars(1), EagerMode(), WithForceBreakRunes(5))

	sentence, ok := seg.Feed("abcde")
	if !ok {
		t.Fatal("expected force break at 5 runes")
	}
	if sentence != "abcde" {
		t.Fatalf("sentence = %q, want abcde", sentence)
	}
}

func TestSegmenter_Flush_Empty(t *testing.T) {
	seg := NewSegmenter()
	if s := seg.Flush(); s != "" {
		t.Fatalf("Flush() = %q, want empty", s)
	}
}

func TestSegmenter_Flush_Remainder(t *testing.T) {
	seg := NewSegmenter()
	seg.Feed("no break")
	s := seg.Flush()
	if s != "no break" {
		t.Fatalf("Flush() = %q, want %q", s, "no break")
	}
}
