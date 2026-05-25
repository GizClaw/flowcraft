package words

import "testing"

func TestQuestionCues(t *testing.T) {
	if !HasTemporalQuestionCue("When did Alice travel?") {
		t.Fatal("expected temporal cue")
	}
	if !HasTemporalQuestionCue("Alice 最早什么时候 moved?") {
		t.Fatal("expected multilingual temporal cue")
	}
	if !HasIntentTemporalQuestionCue("How many months did it take?") {
		t.Fatal("expected intent temporal cue")
	}
	if HasIntentTemporalQuestionCue("What is Alice's favorite city?") {
		t.Fatal("unexpected temporal intent cue")
	}
	if !HasNumericIntentCue("How many pets does Alice have?") {
		t.Fatal("expected numeric cue")
	}
}

func TestEntityStopwords(t *testing.T) {
	for _, tok := range []string{"who", "met", "said"} {
		if !IsIntentEntityStopword(tok) {
			t.Fatalf("%q should be an intent entity stopword", tok)
		}
	}
	for _, tok := range []string{"she", "okay"} {
		if !IsStructurizerEntityStopword(tok) {
			t.Fatalf("%q should be a structurizer entity stopword", tok)
		}
	}
	if IsStructurizerEntityStopword("Will") {
		t.Fatal("modal homograph Will must remain available as a name")
	}
}

func TestLooksProcedural(t *testing.T) {
	cases := []string{
		"When comparing options, use a markdown table.",
		"Before processing invoices, run OCR and then extract entities.",
		"Always check cache before calling the API.",
		"Prefer markdown output for answers.",
	}
	for _, c := range cases {
		if !LooksProcedural(c) {
			t.Fatalf("expected procedural cue for %q", c)
		}
	}
	if LooksProcedural("Alice prefers tea in the morning.") {
		t.Fatal("simple preference text should not look procedural")
	}
}
