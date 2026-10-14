package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestBudgetOffPromptIsThePreBudgetPrompt pins the promise the budget makes: with
// the budget off, the extractor sends exactly the prompt 1.5.0 sent, so a
// workspace that opts out derives what it derived before the budget existed.
// Prompt text is not part of the derivation policy digest, which is why this
// hash exists: an intentional prompt edit belongs with an AlgorithmVersion bump,
// and this test is what makes the edit deliberate.
//
// The digest is taken over normalised line endings. The template is embedded, so
// the prompt a build renders carries whatever newline the checkout used -- CRLF
// on Windows -- and the pin is about the prompt's words rather than about the
// platform it was built on.
func TestBudgetOffPromptIsThePreBudgetPrompt(t *testing.T) {
	for _, pinned := range []struct {
		name   string
		text   string
		digest string
	}{
		{"simple", factSystem, "9cdab38383108c604347141bd01574351a19cd22758c104898b529ca4113bd8a"},
		{"rich", factSystemRich, "96208767c77177ee8c9ee2febf03ca18daa7a718ebb35f644be23c12bab5cdba"},
	} {
		if got := promptDigest(pinned.text); got != pinned.digest {
			t.Errorf("%s prompt digest = %s, want %s", pinned.name, got, pinned.digest)
		}
		if strings.Contains(pinned.text, "BUDGET") || strings.Contains(pinned.text, "priority") {
			t.Errorf("%s prompt describes the budget with the budget off", pinned.name)
		}
	}
}

func promptDigest(prompt string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(prompt, "\r\n", "\n")))
	return hex.EncodeToString(sum[:])
}

// TestQuotaPromptNamesTheBudgetAndItsPriority covers the other side: the quota
// prompt tells the model how many facts the turn may add and what the two
// priority values mean, and the count it names is the configured one rather than
// a default.
func TestQuotaPromptNamesTheBudgetAndItsPriority(t *testing.T) {
	prompt := factSystemFor(StrategySimple, true, 23)
	for _, needle := range []string{"at most 23 facts", `"priority"`, `"high"`, `"low"`} {
		if !strings.Contains(prompt, needle) {
			t.Errorf("quota prompt does not mention %q", needle)
		}
	}
	for _, strategy := range []FactStrategy{StrategySimple, StrategyRich} {
		if prompt := factSystemFor(strategy, false, 23); strings.Contains(prompt, "priority") {
			t.Errorf("%s budget-off prompt describes a priority key its schema does not declare", strategy)
		}
	}
}
