package words

import "testing"

func TestSignificantQueryTextDropsQuestionStopwords(t *testing.T) {
	if got := SignificantQueryText("What pets does Jordan have?"); got != "pets Jordan" {
		t.Fatalf("SignificantQueryText = %q", got)
	}
}

func TestStripTemporalQuestionWordsSupportsMultilingualTerms(t *testing.T) {
	cases := map[string]string{
		"¿Cuándo fue Alice a Madrid?": "Alice a Madrid",
		"Quand Alice était à Paris?":  "Alice à Paris",
		"Wann war Alice in Berlin?":   "Alice in Berlin",
		"Когда Алиса была в Москве?":  "Алиса в Москве",
	}
	for query, want := range cases {
		if got := StripTemporalQuestionWords(query); got != want {
			t.Fatalf("StripTemporalQuestionWords(%q) = %q, want %q", query, got, want)
		}
	}
}
