package kanban

import (
	"fmt"
	"strings"
)

// TaskContext renders a card as prose for a language model: what was
// asked, why it was delegated, and how it turned out.
//
// This lives here rather than in the tool layer because the rendering
// must stay in step with the card's state machine — a new [Status]
// needs a new sentence — and because more than one surface (a tool, a
// prompt preamble, a debug view) wants the identical text.
//
// It is deliberately a projection, not a protocol. Nothing parses the
// output; agents learn about completed work by observing the board or
// asking for a card, never by pattern-matching a rendered string.
func (c *Card) TaskContext() string {
	if c == nil {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Task %s\n\n", c.ID)

	if c.Task != nil {
		if c.Task.UserQuery != "" {
			b.WriteString("### Original request\n")
			fmt.Fprintf(&b, "%s\n\n", c.Task.UserQuery)
		}
		if c.Task.DispatchNote != "" {
			b.WriteString("### Dispatch note\n")
			fmt.Fprintf(&b, "%s\n\n", c.Task.DispatchNote)
		}
		b.WriteString("### Instruction\n")
		fmt.Fprintf(&b, "Target agent: %s\n", c.Task.TargetAgentID)
		fmt.Fprintf(&b, "%s\n\n", c.Task.Query)
	}

	b.WriteString("### Status\n")
	b.WriteString(c.statusProse())
	return b.String()
}

// statusProse explains a card's state in terms of what the reader
// should do next, which is the only reason a model is reading it.
func (c *Card) statusProse() string {
	switch c.Status {
	case StatusPending:
		return "Pending — not claimed yet. Wait; do not resubmit.\n"
	case StatusClaimed:
		return fmt.Sprintf("Running on %q. Wait; do not resubmit.\n", c.Consumer)
	case StatusSuspended:
		return "Suspended — execution paused awaiting an external signal " +
			"and will continue when resumed. Wait; do not resubmit.\n"
	case StatusDone:
		out := ""
		if c.Result != nil {
			out = c.Result.Output
		}
		return fmt.Sprintf("Completed.\n\n%s\n", out)
	case StatusFailed:
		msg := ""
		if c.Result != nil {
			msg = c.Result.Error
		}
		return fmt.Sprintf("Failed: %s\n", msg)
	case StatusCancelled:
		reason := ""
		if c.Result != nil {
			reason = c.Result.Error
		}
		return fmt.Sprintf("Cancelled: %s\n", reason)
	}
	return fmt.Sprintf("Unknown status %q.\n", c.Status)
}
