package timex

import "testing"

func TestIsRelativePhrase(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"yesterday", true},
		{"next month", true},
		{"4 years ago", true},
		{"two weekends ago", true},
		{"in three weeks", true},
		{"hace dos semanas", true},
		{"dans trois semaines", true},
		{"la próxima semana", true},
		{"la semaine prochaine", true},
		{"nächste woche", true},
		{"próxima semana", true},
		{"volgende week", true},
		{"на следующей неделе", true},
		{"四年前", true},
		{"两个月后", true},
		{"三周前", true},
		{"2024-05-07", false},
		{"May 7, 2024", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsRelativePhrase(c.raw); got != c.want {
			t.Errorf("IsRelativePhrase(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestFindRelativePhraseReturnsCountedAgoSpan(t *testing.T) {
	match := FindRelativePhrase("Melanie went camping two weekends ago.")
	if match == nil {
		t.Fatal("expected counted-ago relative phrase")
	}
	if match.Text != "two weekends ago" {
		t.Fatalf("match text = %q, want two weekends ago", match.Text)
	}
}

func TestFindRelativePhraseReturnsCountedFutureSpan(t *testing.T) {
	match := FindRelativePhrase("The trip starts in three weeks.")
	if match == nil {
		t.Fatal("expected counted-future relative phrase")
	}
	if match.Text != "in three weeks" {
		t.Fatalf("match text = %q, want in three weeks", match.Text)
	}
}

func TestFindRelativePhraseReturnsMultilingualCountedSpan(t *testing.T) {
	cases := []string{
		"hace dos semanas",
		"dans trois semaines",
		"两个月后",
	}
	for _, want := range cases {
		match := FindRelativePhrase("prefix " + want + " suffix")
		if match == nil {
			t.Fatalf("expected counted relative phrase for %q", want)
		}
		if match.Text != want {
			t.Fatalf("match text = %q, want %q", match.Text, want)
		}
	}
}
