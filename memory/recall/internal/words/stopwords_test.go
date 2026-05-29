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
	if IsWeakExtractorEntityPhrase([]string{"pottery", "class", "yesterday"}) {
		t.Fatal("concrete entity phrases should not be weak extractor entity phrases")
	}
	for _, phrase := range [][]string{{"to", "help", "others"}, {"being", "accepted"}, {"planning", "a", "trip"}, {"taking", "care", "of", "family"}, {"ayudando", "otros"}, {"aider", "les", "autres"}, {"helfen", "anderen"}, {"ajudando", "outros"}, {"помогать", "другим"}, {"帮助", "别人"}} {
		if !IsWeakExtractorRelationObjectPhrase(phrase) {
			t.Fatalf("%q should be a weak relation object phrase", phrase)
		}
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
	if IsWeakExtractorEntityPhrase([]string{"the", "pottery", "class"}) {
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
	for _, text := range []string{"That sounds awesome.", "Congrats on the race!", "Gracias por compartir", "Merci, c'est gentil", "Klingt toll", "Спасибо за это", "听起来很棒"} {
		if !IsLowValueExtractorNoteText(text) {
			t.Fatalf("%q should be low-value extractor note language", text)
		}
	}
	if IsLowValueExtractorNoteText("Avery bought a ceramic bowl.") {
		t.Fatal("concrete fact text should not be low-value extractor note language")
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
	for _, phrase := range [][]string{{"difference"}, {"community", "work"}, {"check", "up"}, {"medal"}, {"donations"}, {"plans"}, {"toy", "drive"}, {"own", "family"}, {"focused", "business"}} {
		if !IsAbstractMadeRelationObjectPhrase(phrase) {
			t.Fatalf("%q should be abstract for made relation objects", phrase)
		}
	}
	if IsAbstractMadeRelationObjectPhrase([]string{"model", "bridge"}) {
		t.Fatal("concrete made relation object should not be abstract")
	}
}
