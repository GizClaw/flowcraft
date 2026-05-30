package domain

// ForgetMode selects how Memory.Forget / ForgetAll retire facts.
type ForgetMode string

const (
	// ForgetSoft marks facts Closed without removing ledger rows
	// (Retract-equivalent; History still sees the chain).
	ForgetSoft ForgetMode = "soft"
	// ForgetHard removes facts from the canonical store and
	// projections (today's default Forget behaviour).
	ForgetHard ForgetMode = "hard"
)

// NormalizeForgetMode returns Hard for empty input.
func NormalizeForgetMode(m ForgetMode) ForgetMode {
	if m == ForgetSoft {
		return ForgetSoft
	}
	return ForgetHard
}
