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
	if IsIntentEntityStopword("will") {
		t.Fatal("intent entity stopwords should preserve Will as a possible entity mention")
	}
}

func TestInvalidEntityAnchorTokens(t *testing.T) {
	for _, tok := range []string{"on", "into", "as", "para", "avec", "zu", "voor", "для"} {
		if !IsInvalidEntityAnchorToken(tok) {
			t.Fatalf("%q should be an invalid function-word entity anchor", tok)
		}
	}
	for _, tok := range []string{"today", "next", "ago", "mañana", "demain", "gestern", "hoje", "gisteren", "завтра", "今天"} {
		if !IsInvalidEntityAnchorToken(tok) {
			t.Fatalf("%q should be an invalid relative-time entity anchor", tok)
		}
	}
	for _, tok := range []string{"July", "Monday"} {
		if !IsInvalidEntityAnchorToken(tok) {
			t.Fatalf("%q should be an invalid calendar entity anchor", tok)
		}
	}
	for _, tok := range []string{"Will", "Avery", "Riverton"} {
		if IsInvalidEntityAnchorToken(tok) {
			t.Fatalf("%q should remain available as an entity anchor", tok)
		}
	}
}
