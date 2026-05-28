package words

import "testing"

func TestEntityStopwords(t *testing.T) {
	for _, tok := range []string{"who", "met", "said", "cuándo", "quand", "wann", "quando", "wanneer", "когда", "什么时候"} {
		if !IsIntentEntityStopword(tok) {
			t.Fatalf("%q should be an intent entity stopword", tok)
		}
	}
	for _, tok := range []string{"she", "okay", "ella", "elle", "sie", "você", "jij", "она", "我们"} {
		if !IsStructurizerEntityStopword(tok) {
			t.Fatalf("%q should be a structurizer entity stopword", tok)
		}
	}
	if IsStructurizerEntityStopword("Will") {
		t.Fatal("modal homograph Will must remain available as a name")
	}
}

func TestExtractorEntityWeakTokens(t *testing.T) {
	for _, tok := range []string{"on", "into", "as"} {
		if !IsExtractorEntityFunctionWord(tok) {
			t.Fatalf("%q should be an extractor entity function word", tok)
		}
	}
	for _, tok := range []string{"being", "taking", "finding", "working", "writing"} {
		if !IsExtractorAbstractGerundEntityToken(tok) {
			t.Fatalf("%q should be an extractor abstract gerund token", tok)
		}
	}
	for _, phrase := range [][]string{{"planning", "to", "repair", "a", "bicycle"}, {"enough", "to", "finish", "the", "fundraiser"}, {"i", "m"}, {"we", "re"}, {"working", "on", "cars"}} {
		if !IsWeakExtractorEntityPhrase(phrase) {
			t.Fatalf("%q should be a weak extractor entity phrase", phrase)
		}
	}
	if IsWeakExtractorEntityPhrase([]string{"pottery", "class", "yesterday"}) {
		t.Fatal("concrete entity phrases should not be weak extractor entity phrases")
	}
	for _, subject := range [][]string{{"i"}, {"me"}, {"my"}, {"i", "m"}, {"i", "ll"}} {
		if !IsFirstPersonSingularExtractorSubject(subject) {
			t.Fatalf("%q should be a first-person singular extractor subject", subject)
		}
	}
	for _, subject := range [][]string{{"we"}, {"they"}, {"alice"}} {
		if IsFirstPersonSingularExtractorSubject(subject) {
			t.Fatalf("%q should not be a first-person singular extractor subject", subject)
		}
	}
	for _, tok := range []string{"today", "next", "ago"} {
		if !IsRelativeTimeEntityToken(tok) {
			t.Fatalf("%q should be a relative-time entity token", tok)
		}
	}
	for _, tok := range []string{"July", "Monday"} {
		if !IsCalendarEntityToken(tok) {
			t.Fatalf("%q should be a calendar entity token", tok)
		}
	}
}
