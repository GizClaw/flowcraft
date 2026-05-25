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
