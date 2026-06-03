package words

import "testing"

func TestCommonGraphNouns(t *testing.T) {
	for _, noun := range []string{"user", "person", "today", "they", "persona", "personne", "benutzer", "pessoa", "gebruiker", "человек", "用户"} {
		if !IsCommonGraphNoun(noun) {
			t.Fatalf("%q should be a common graph noun", noun)
		}
	}
	if IsCommonGraphNoun("alice") {
		t.Fatal("specific entity should not be a common graph noun")
	}
	if IsCommonGraphNoun("person archive") {
		t.Fatal("common graph noun filter must only match the whole canonical node")
	}
}
