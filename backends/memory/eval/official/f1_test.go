package official

import (
	"math"
	"testing"
)

func TestF1MatchesTheReferenceSemantics(t *testing.T) {
	for _, test := range []struct {
		name       string
		prediction string
		gold       string
		want       float64
	}{
		{"identical", "7 May 2023", "7 May 2023", 1},
		{"articles and case ignored", "The camping trip", "camping trip", 1},
		{"stemming", "Melanie painted pictures", "Melanie paints a picture", 1},
		{"punctuation ignored", "Sweden, and Norway", "sweden norway", 1},
		{"partial overlap", "Caroline adopted a dog", "Caroline adopted a cat", 2.0 / 3.0},
		{"disjoint", "pizza", "bicycle", 0},
		{"empty prediction", "", "anything", 0},
	} {
		if got := F1(test.prediction, test.gold); math.Abs(got-test.want) > 0.001 {
			t.Fatalf("%s: F1(%q, %q) = %.4f, want %.4f", test.name, test.prediction, test.gold, got, test.want)
		}
	}
}

func TestScoreFollowsTheCategoryRules(t *testing.T) {
	// Category 1 averages the best F1 of each comma-separated gold answer.
	multiHop := Score(1, "roast marshmallows, tell stories", []string{"Roast marshmallows, tell stories"})
	if multiHop < 0.99 {
		t.Fatalf("category 1 = %.3f, want the sub-answers to be scored separately", multiHop)
	}
	// One of two gold sub-answers covered must score 0.5, not 1.
	if partial := Score(1, "roast marshmallows", []string{"Roast marshmallows, tell stories"}); math.Abs(partial-0.5) > 0.001 {
		t.Fatalf("category 1 partial = %.3f, want 0.5", partial)
	}
	// Category 3 grades against the first ";" segment only.
	if got := Score(3, "Nintendo Switch", []string{"A Nintendo Switch; since the game is exclusive"}); got < 0.99 {
		t.Fatalf("category 3 = %.3f, want the first segment to match", got)
	}
	// Category 5 is the refusal check, independent of overlap.
	for _, refusal := range []string{"No information available.", "That is not mentioned in the conversation."} {
		if got := Score(5, refusal, nil); got != 1 {
			t.Fatalf("category 5 refused with %q = %.1f, want 1", refusal, got)
		}
	}
	if got := Score(5, "Caroline went to Sweden", nil); got != 0 {
		t.Fatalf("category 5 answered = %.1f, want 0", got)
	}
}
