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

func TestExtractorEntityWeakTokens(t *testing.T) {
	for _, tok := range []string{"on", "into", "as", "para", "avec", "zu", "voor", "для"} {
		if !IsExtractorEntityFunctionWord(tok) {
			t.Fatalf("%q should be an extractor entity function word", tok)
		}
	}
	for _, tok := range []string{"will", "her", "the"} {
		if IsExtractorEntityFunctionWord(tok) || IsExtractorAbstractGerundEntityToken(tok) {
			t.Fatalf("%q should not be inherited into extractor semantic token dictionaries", tok)
		}
	}
	for _, tok := range []string{"being", "taking", "finding", "working", "writing", "intentando", "essayer", "versuchen", "planejando", "proberen", "планировать", "计划"} {
		if !IsExtractorAbstractGerundEntityToken(tok) {
			t.Fatalf("%q should be an extractor abstract gerund token", tok)
		}
	}
	for _, phrase := range [][]string{{"planning", "to", "repair", "a", "bicycle"}, {"enough", "to", "finish", "the", "fundraiser"}, {"i", "m"}, {"we", "re"}, {"working", "on", "cars"}, {"tratando", "de", "ayudar"}, {"capable", "de", "finir"}, {"versuchen", "zu", "helfen"}, {"tentando", "ajudar"}, {"планируя", "поездку"}, {"计划", "旅行"}} {
		if !IsWeakExtractorEntityPhrase(phrase) {
			t.Fatalf("%q should be a weak extractor entity phrase", phrase)
		}
	}
	if IsWeakExtractorEntityPhrase([]string{"woodworking", "class", "yesterday"}) {
		t.Fatal("concrete entity phrases should not be weak extractor entity phrases")
	}
	if IsWeakExtractorRelationObjectPhrase([]string{"ceramic", "bowl"}) {
		t.Fatal("concrete relation objects should not be weak relation object phrases")
	}
	if IsWeakExtractorRelationObjectPhrase([]string{"her", "cat"}) {
		t.Fatal("relation object start-token dictionary should not inherit broad English stopwords")
	}
	if IsWeakExtractorRelationObjectPhrase([]string{"su", "perro"}) {
		t.Fatal("relation object checks should preserve concrete multilingual objects after determiners")
	}
	if IsWeakExtractorEntityPhrase([]string{"the", "woodworking", "class"}) {
		t.Fatal("entity phrase checks should not treat all English stopwords as abstract gerunds")
	}
	for _, subject := range [][]string{{"i"}, {"me"}, {"my"}, {"i", "m"}, {"i", "ll"}, {"yo"}, {"je"}, {"ich"}, {"eu"}, {"ik"}, {"я"}, {"我"}} {
		if !IsFirstPersonSingularExtractorSubject(subject) {
			t.Fatalf("%q should be a first-person singular extractor subject", subject)
		}
	}
	for _, subject := range [][]string{{"we"}, {"they"}, {"alice"}} {
		if IsFirstPersonSingularExtractorSubject(subject) {
			t.Fatalf("%q should not be a first-person singular extractor subject", subject)
		}
	}
	for _, tok := range []string{"today", "next", "ago", "mañana", "demain", "gestern", "hoje", "gisteren", "завтра", "今天"} {
		if !IsRelativeTimeEntityToken(tok) {
			t.Fatalf("%q should be a relative-time entity token", tok)
		}
	}
	for _, tok := range []string{"the", "will", "her"} {
		if IsRelativeTimeEntityToken(tok) {
			t.Fatalf("%q should not be inherited into relative-time tokens", tok)
		}
	}
	for _, tok := range []string{"July", "Monday"} {
		if !IsCalendarEntityToken(tok) {
			t.Fatalf("%q should be a calendar entity token", tok)
		}
	}
	for _, tok := range []string{"went", "bought", "visited", "could"} {
		if !IsSafeFirstPersonExtractorContentVerb(tok) {
			t.Fatalf("%q should be safe for first-person content rewrite", tok)
		}
	}
	if got, ok := ThirdPersonExtractorContentVerb("prefer"); !ok || got != "prefers" {
		t.Fatalf("prefer third-person rewrite = %q/%v, want prefers/true", got, ok)
	}
	if !IsUnsupportedFirstPersonExtractorContentStart([]string{"my", "apartment"}) {
		t.Fatal("lowercase possessive content starts should be treated as first-person")
	}
	if !IsUnsupportedFirstPersonExtractorContentStart([]string{"me"}) || !IsUnsupportedFirstPersonExtractorContentStart([]string{"ourselves"}) {
		t.Fatal("embedded first-person residue tokens should be treated as unsupported")
	}
}
