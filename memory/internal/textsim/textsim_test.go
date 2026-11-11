package textsim

import "testing"

// TestJaccardAndContainmentAnswerDifferentQuestions is the reason both measures
// exist: a restatement that adds a trailing clause is not very similar by
// Jaccard, because the added tokens grow the union, but the shorter text is
// already inside the longer one -- and it is still one claim.
func TestJaccardAndContainmentAnswerDifferentQuestions(t *testing.T) {
	restatement := "On 12 March 2023, Nadia joined a community choir in Bristol."
	expanded := "On 12 March 2023, Nadia joined a community choir in Bristol and stayed for the whole rehearsal."
	if jaccard := Jaccard(restatement, expanded); jaccard >= 0.7 {
		t.Fatalf("jaccard = %v, want an added clause to pull the shared share down", jaccard)
	}
	if containment := Containment(restatement, expanded); containment < 0.9 {
		t.Fatalf("containment = %v, want the shorter text contained in the longer one", containment)
	}
	// The measures agree on the extremes.
	if got := Jaccard(restatement, restatement); got != 1 {
		t.Fatalf("jaccard of a text with itself = %v", got)
	}
	if got := Containment(expanded, restatement); got != Containment(restatement, expanded) {
		t.Fatalf("containment is not symmetric in its arguments: %v", got)
	}
}

// TestMeasuresAreZeroForEmptyAndNonZeroForNonLatinText covers the two input
// shapes the module has to survive: an empty or tokenless text is never "similar"
// to anything (a zero Jaccard there would otherwise read as a perfect match), and
// a CJK restatement has to compare as related -- the tokens are the retrieval
// splitter's, so text without whitespace word boundaries compares by character
// and bigram overlap instead of reading as two unrelated strings.
func TestMeasuresAreZeroForEmptyAndNonZeroForNonLatinText(t *testing.T) {
	if got := Jaccard("", ""); got != 0 {
		t.Fatalf("jaccard of two empty texts = %v, want 0", got)
	}
	if got := Containment("   ", "Nadia joined a choir."); got != 0 {
		t.Fatalf("containment of a tokenless text = %v, want 0", got)
	}
	if got := Similarity(nil, map[string]struct{}{"nadia": {}}); got != 0 {
		t.Fatalf("similarity with an empty set = %v, want 0", got)
	}
	if got := Jaccard("纳迪亚加入了社区合唱团", "纳迪亚加入合唱团"); got == 0 {
		t.Fatal("a CJK restatement reads as unrelated; the measures must use the retrieval tokenizer")
	}
}
