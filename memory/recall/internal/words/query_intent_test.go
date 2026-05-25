package words

import "testing"

func TestQueryIntentLexiconTemporalMultilingual(t *testing.T) {
	cases := []string{
		"By when did Alice finish the workshop?",
		"What year did Bob move?",
		"¿Desde cuándo vive Alice allí?",
		"Quelle date Melanie a-t-elle montrée?",
		"Seit wann arbeitet Alice dort?",
		"Quando Alice viajou?",
		"Sinds wanneer werkt Alice daar?",
		"Когда Alice переехала?",
		"Alice 是哪一年搬家的?",
	}
	for _, c := range cases {
		if !HasTemporalQuestionCue(c) {
			t.Fatalf("expected temporal cue for %q", c)
		}
	}
	if HasTemporalQuestionCue("Before processing invoices, run OCR.") {
		t.Fatal("procedural before should not be temporal question intent")
	}
}

func TestQueryIntentLexiconDurationMultilingual(t *testing.T) {
	cases := []string{
		"How long did the trip last?",
		"¿Cuánto tiempo duró el viaje?",
		"Combien de temps le voyage a-t-il duré?",
		"Wie lange dauerte die Reise?",
		"Quanto tempo durou a viagem?",
		"Hoe lang duurde de reis?",
		"Как долго длилась поездка?",
		"这次旅行持续了多久?",
	}
	for _, c := range cases {
		if !HasDurationQuestionCue(c) {
			t.Fatalf("expected duration cue for %q", c)
		}
	}
}

func TestQueryIntentLexiconNumericMultilingual(t *testing.T) {
	cases := []string{
		"How often did Alice visit?",
		"What percentage did she mention?",
		"¿Cuántas veces visitó Alice?",
		"Combien de fois Alice est-elle venue?",
		"Wie oft kam Alice vorbei?",
		"Quantas vezes Alice visitou?",
		"Hoe vaak kwam Alice langs?",
		"Сколько раз Alice приходила?",
		"Alice 去过几次?",
		"这是第几次会议?",
	}
	for _, c := range cases {
		if !HasNumericIntentCue(c) {
			t.Fatalf("expected numeric cue for %q", c)
		}
	}
	if HasNumericIntentCue("Open the account settings") {
		t.Fatal("count must not match account")
	}
}

func TestSubjectInferenceCue(t *testing.T) {
	for _, q := range []string{"Who did Alice meet?", "Alice's favorite city"} {
		if !HasSubjectInferenceCue(q) {
			t.Fatalf("expected subject inference cue for %q", q)
		}
	}
	if HasSubjectInferenceCue("Alice went to Paris.") {
		t.Fatal("plain statement should not infer query subject")
	}
}
