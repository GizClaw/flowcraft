package words

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestSignificantQueryTextDropsQuestionStopwords(t *testing.T) {
	if got := SignificantQueryText("What pets does Melanie have?"); got != "pets Melanie" {
		t.Fatalf("SignificantQueryText = %q", got)
	}
}

func TestBridgeClausesSplitsSurfaceConnectors(t *testing.T) {
	got := BridgeClauses("Where did Alice buy the necklace that she wore?")
	want := []string{"Where did Alice buy the necklace", "she wore?"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BridgeClauses = %#v, want %#v", got, want)
	}
}

func TestCollectionAnchorWordsKeepsTitleCaseNonStopwords(t *testing.T) {
	got := CollectionAnchorWords("What pets does Melanie have?")
	want := []string{"Melanie"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CollectionAnchorWords = %#v, want %#v", got, want)
	}
}

func TestSurfaceCueHelpers(t *testing.T) {
	tokens := map[string]struct{}{"pet": {}, "melanie": {}}
	if !HasCollectionSurfaceCue("What pets does Melanie have?", tokens, nil) {
		t.Fatal("expected collection cue")
	}
	if !HasCollectionSurfaceCue("How many times did Alice visit?", nil, []domain.QueryNumericIntentKind{domain.QueryNumericIntentFrequency}) {
		t.Fatal("expected numeric collection cue")
	}
	if !HasBridgeSurfaceCue("Where did Alice buy the necklace that she wore?", map[string]struct{}{"alice": {}}) {
		t.Fatal("expected bridge cue")
	}
}

func TestSurfaceCueHelpersRespectTokenBoundaries(t *testing.T) {
	if HasCollectionSurfaceCue("Whatever Alice bought was blue.", map[string]struct{}{"item": {}}, nil) {
		t.Fatal("substring inside whatever must not trigger what collection cue")
	}
	if HasBridgeSurfaceCue("The theater was busy.", nil) {
		t.Fatal("substring inside theater must not trigger her bridge cue")
	}
}

func TestSurfaceCueHelpersSupportMultilingualCollection(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		tokens map[string]struct{}
	}{
		{name: "spanish possession", query: "¿Qué mascotas tiene Melanie?", tokens: map[string]struct{}{"mascota": {}, "melanie": {}}},
		{name: "french count", query: "Combien de livres Alice a-t-elle?", tokens: map[string]struct{}{"livre": {}, "alice": {}}},
		{name: "german which", query: "Welche Bücher hat Alice?", tokens: map[string]struct{}{"buch": {}, "alice": {}}},
		{name: "chinese plural literal", query: "Melanie 有哪些宠物?", tokens: map[string]struct{}{"melanie": {}}},
		{name: "chinese what with collection cue", query: "Melanie 有什么宠物?", tokens: map[string]struct{}{"melanie": {}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !HasCollectionSurfaceCue(tc.query, tc.tokens, nil) {
				t.Fatalf("expected collection cue for %q", tc.query)
			}
		})
	}
}

func TestSurfaceCueHelpersSupportMultilingualBridge(t *testing.T) {
	cases := []string{
		"¿Dónde compró Alice el collar que llevaba?",
		"Où Alice a-t-elle acheté le collier qu'elle portait?",
		"Wo kaufte Alice die Halskette, welche sie trug?",
		"Где Алиса купила ожерелье, которое она носила?",
		"Alice 买了她戴的项链之前去了哪里?",
	}
	for _, query := range cases {
		if !HasBridgeSurfaceCue(query, map[string]struct{}{"alice": {}}) {
			t.Fatalf("expected bridge cue for %q", query)
		}
	}
}

func TestSurfaceCueHelpersAvoidBroadMultilingualBridgeCues(t *testing.T) {
	cases := []string{
		"Die Alice mag Musik.",
		"Das Buch ist blau.",
		"Wie spät ist es?",
		"Ik woon voor het station.",
		"Ik ga na school naar huis.",
	}
	for _, query := range cases {
		if HasBridgeSurfaceCue(query, nil) {
			t.Fatalf("broad function word must not trigger bridge cue for %q", query)
		}
	}
}

func TestSurfaceCueHelpersDoNotTreatChineseWhatAsCollectionAlone(t *testing.T) {
	if HasCollectionSurfaceCue("Alice 喜欢什么?", map[string]struct{}{"alice": {}}, nil) {
		t.Fatal("什么 alone should be direct lookup, not set completion")
	}
}

func TestSurfaceCueHelpersSupportDisambiguation(t *testing.T) {
	cases := []string{
		"Alice or Bob?",
		"Did Alice choose tea instead?",
		"Did Alice pick tea rather than coffee?",
		"Which one did Alice prefer?",
		"Alice 选茶还是咖啡?",
	}
	for _, query := range cases {
		if !HasDisambiguationSurfaceCue(query) {
			t.Fatalf("expected disambiguation cue for %q", query)
		}
	}
	if HasDisambiguationSurfaceCue("Alice went to Paris.") {
		t.Fatal("plain statement should not trigger disambiguation")
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
