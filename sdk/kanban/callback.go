package kanban

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BuildCallbackQuery assembles a concise callback message from a completed card.
// It includes a summary and card_id but not the full task context; Dispatcher
// can call task_context(card_id) to retrieve full details if needed.
func BuildCallbackQuery(card *Card, result *ResultPayload) string {
	p := PayloadMap(card.Payload)

	var b strings.Builder
	fmt.Fprintf(&b, "[Task Callback] card_id=%s\n\n", card.ID)
	fmt.Fprintf(&b, "Target Agent: %s\n", p["target_agent_id"])

	if result.Error != "" {
		b.WriteString("Status: failed\n")
		fmt.Fprintf(&b, "Error: %s\n", result.Error)
	} else {
		b.WriteString("Status: completed\n")
		output := result.Output
		if len(output) > 200 {
			output = output[:200] + "..."
		}
		fmt.Fprintf(&b, "Summary: %s\n", output)
	}

	fmt.Fprintf(&b, "\nUse task_context(card_id=%q) to recall the original request and your dispatch note.", card.ID)
	return b.String()
}

// BuildTaskContext assembles the full task context from a card for the
// task_context tool. The card's Payload (after Done) is a map[string]any
// containing the original dispatch fields and execution result.
func BuildTaskContext(card *Card) string {
	p := PayloadMap(card.Payload)

	var b strings.Builder
	fmt.Fprintf(&b, "## Task Context (%s)\n\n", card.ID)

	if uq, _ := p["user_query"].(string); uq != "" {
		b.WriteString("### Original Request\n")
		fmt.Fprintf(&b, "User: %s\n\n", uq)
	}

	if dn, _ := p["dispatch_note"].(string); dn != "" {
		b.WriteString("### Dispatch Note\n")
		b.WriteString(dn)
		b.WriteString("\n\n")
	}

	b.WriteString("### Task Instruction\n")
	fmt.Fprintf(&b, "Target Agent: %s\n", p["target_agent_id"])
	fmt.Fprintf(&b, "Instruction: %s\n\n", p["query"])

	b.WriteString("### Execution Result\n")
	switch card.Status {
	case CardPending:
		b.WriteString("Status: pending (task not yet claimed, wait for callback)\n")
	case CardClaimed:
		b.WriteString("Status: running (agent is processing, wait for callback, do not resubmit)\n")
	case CardFailed:
		fmt.Fprintf(&b, "Status: failed\nError: %s\n", card.Error)
	case CardDone:
		output, _ := p["output"].(string)
		fmt.Fprintf(&b, "Status: completed\n%s\n", output)
	}

	return b.String()
}

// CompactCallbackForMemory compresses a callback message for memory persistence.
// It truncates the output to reduce token usage in the memory window while
// preserving the card_id and status for future task_context lookups.
func CompactCallbackForMemory(callbackMsg string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 300
	}
	if len(callbackMsg) <= maxLen {
		return callbackMsg
	}

	lines := strings.Split(callbackMsg, "\n")
	var b strings.Builder
	for i, line := range lines {
		next := len(line)
		if i > 0 {
			next++ // for the newline separator
		}
		if b.Len()+next > maxLen {
			b.WriteString("\n... (use `task_context` for full details)")
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

// IsCallbackMessage returns true if the message text is a Kanban task callback.
func IsCallbackMessage(text string) bool {
	return strings.HasPrefix(text, "[Task Callback]")
}

// PayloadMap safely converts a Card.Payload to map[string]any.
// After Done(), Payload is map[string]any (direct assertion).
// Before Done() (Pending/Claimed), Payload is a TaskPayload struct (JSON round-trip).
func PayloadMap(payload any) map[string]any {
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}
