package answer

import (
	"strings"
	"testing"
)

// TestEvidenceAnswerKeepsOnlyTheAnswer pins the parser the evidence style
// depends on: the token-F1 scorer and the judges must see the short answer,
// not the quoted scratch work. A formatting slip falls back to the raw reply
// instead of failing the question.
func TestEvidenceAnswerKeepsOnlyTheAnswer(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "two lines",
			raw:  "Quotes: \"Caroline went to the pottery class on 2 July 2023\"\nShort answer: 2 July 2023",
			want: "2 July 2023",
		},
		{
			name: "answer wraps onto a second line",
			raw:  "Quotes: none\nShort answer: Ramen,\nsushi",
			want: "Ramen,\nsushi",
		},
		{
			name: "missing marker keeps the reply, minus the quotes section",
			raw:  "Quotes: \"the lake sunrise\"\n2022",
			want: "2022",
		},
		{
			name: "plain reply passes through",
			raw:  "7 May 2023",
			want: "7 May 2023",
		},
		{
			name: "marker is matched case-insensitively",
			raw:  "Quotes: none\nSHORT ANSWER: Sweden",
			want: "Sweden",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := evidenceAnswer(test.raw); got != test.want {
				t.Fatalf("evidenceAnswer(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

// TestEvidenceStylePromptAndVersion pins the wiring: the style selects its own
// prompt, asks for the quoting step, and carries a version of its own so run
// fingerprints cannot confuse it with the official short protocol.
func TestEvidenceStylePromptAndVersion(t *testing.T) {
	model := &Model{}
	if got := model.WithAnswerStyle(AnswerStyleEvidence).System(); !strings.Contains(got, "Step 1 - evidence") {
		t.Fatalf("evidence style does not use the evidence prompt: %.120q", got)
	}
	if got := model.WithAnswerStyle(AnswerStyleShort).System(); got != answerShortSystem {
		t.Fatal("short style no longer uses the official short prompt")
	}
	if AnswerEvidencePromptVersion == AnswerShortPromptVersion {
		t.Fatal("the evidence style must not share the short prompt version")
	}
}
