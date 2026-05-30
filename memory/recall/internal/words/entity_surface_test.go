package words

import (
	"reflect"
	"testing"
)

func TestExtractIntentEntityMentions(t *testing.T) {
	got := ExtractIntentEntityMentions(`Who did Alice meet in "New York"?`)
	want := []string{"alice", "new", "new york", "york"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractIntentEntityMentions = %#v, want %#v", got, want)
	}
}

func TestExtractIntentEntityMentionsKeepsNamePunctuation(t *testing.T) {
	got := ExtractIntentEntityMentions("Did Jean-Luc call O'Brien?")
	want := []string{"jean-luc", "o'brien"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractIntentEntityMentions = %#v, want %#v", got, want)
	}
}

func TestExtractIntentEntityMentionsSupportsCJKRuns(t *testing.T) {
	got := ExtractIntentEntityMentions("Alice 认识李雷吗?")
	want := []string{"alice", "认识李雷吗"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractIntentEntityMentions = %#v, want %#v", got, want)
	}
}
