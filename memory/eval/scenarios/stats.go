package scenarios

import "fmt"

// Stats reports how much of the upstream corpus survived normalization.
// Everything dropped is intentional and visible.
type Stats struct {
	Conversations     int
	Questions         int
	Turns             int
	DroppedEvidence   int
	SkippedNullAnswer int
}

func (s Stats) String() string {
	return fmt.Sprintf(
		"%d conversations, %d turns, %d questions; dropped %d evidence refs, skipped %d null-answer questions",
		s.Conversations, s.Turns, s.Questions,
		s.DroppedEvidence, s.SkippedNullAnswer,
	)
}
