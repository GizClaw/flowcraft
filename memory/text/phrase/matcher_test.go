package phrase

import "testing"

func TestMatcherContainsPhraseUsesTokenBoundaries(t *testing.T) {
	m := New("How many pets does Alice have?")
	if !m.ContainsPhrase("how", "many") {
		t.Fatal("expected how many phrase")
	}
	if New("Open the account settings").Contains("count") {
		t.Fatal("count must not match account")
	}
}

func TestMatcherFoldsMorphology(t *testing.T) {
	m := New("Before processing invoices, Alice went to run OCR.")
	if !m.Contains("process") {
		t.Fatal("expected processing to fold to process")
	}
	if !m.Contains("go") {
		t.Fatal("expected English irregular went to fold to go")
	}
	if !m.ContainsPhrase("before", "process", "invoice") {
		t.Fatal("expected folded phrase")
	}
}

func TestMatcherFoldsSupportedMultilingualMorphology(t *testing.T) {
	m := New("¿Cuántas opciones revisó Alice?")
	if !m.ContainsPhrase("cuánto", "opción") {
		t.Fatal("expected Spanish morphology to fold across phrase terms")
	}
}

func TestMatcherLiteralForUnsegmentedScripts(t *testing.T) {
	m := New("Alice 最早什么时候 moved?")
	if !m.ContainsLiteral("什么时候") {
		t.Fatal("expected CJK literal cue")
	}
}
