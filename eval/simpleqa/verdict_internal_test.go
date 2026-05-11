package simpleqa

import "testing"

// Production judge models obey "reply with A/B/C" inconsistently. The
// parser must accept the canonical letter forms, the upper-cased word
// forms, and tolerate common framing prefixes ("(A)", "A:", "A.")
// without bucketing anything ambiguous. These cases are derived from
// actual mis-replies we observed during early LongMemEval / SimpleQA
// runs against various providers.
func TestParseVerdict(t *testing.T) {
	cases := []struct {
		in          string
		wantVerdict Verdict
		wantFailed  bool
	}{
		// Canonical single letter.
		{"A", VerdictCorrect, false},
		{"B", VerdictIncorrect, false},
		{"C", VerdictNotAttempted, false},

		// Lower case and surrounding whitespace.
		{"  a  ", VerdictCorrect, false},

		// Framed letter.
		{"A: CORRECT", VerdictCorrect, false},
		{"(B)", VerdictIncorrect, false},
		{"C.", VerdictNotAttempted, false},

		// Word fallback (judge ignored letter instruction).
		{"CORRECT", VerdictCorrect, false},
		{"INCORRECT", VerdictIncorrect, false},
		{"NOT_ATTEMPTED", VerdictNotAttempted, false},
		{"NOT ATTEMPTED", VerdictNotAttempted, false},

		// Letter precedes word — first letter wins (matches the
		// prompt's "just return the letter" instruction).
		{"A: CORRECT, the model named the right children.", VerdictCorrect, false},

		// Unrecognised — must be marked as failed, not silently
		// bucketed as Correct.
		{"", "", true},
		{"the model said maybe", "", true},
		{"Honestly I am not sure", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotV, gotF := parseVerdict(tc.in)
			if gotV != tc.wantVerdict {
				t.Errorf("verdict: want %q, got %q", tc.wantVerdict, gotV)
			}
			if gotF != tc.wantFailed {
				t.Errorf("failed: want %v, got %v", tc.wantFailed, gotF)
			}
		})
	}
}
