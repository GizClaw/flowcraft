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
		{"in three weeks", true},
		{"la próxima semana", true},
		{"la semaine prochaine", true},
		{"nächste woche", true},
		{"próxima semana", true},
		{"volgende week", true},
		{"на следующей неделе", true},
		{"四年前", true},
		{"两个月后", true},
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
