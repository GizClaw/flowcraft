package kanban

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// Subjects emitted by a board, partitioned by card id:
//
//	kanban.card.<cardID>.submitted
//	kanban.card.<cardID>.claimed
//	kanban.card.<cardID>.suspended
//	kanban.card.<cardID>.done
//	kanban.card.<cardID>.failed
//	kanban.card.<cardID>.cancelled
//
// Card id is the partition key because every payload leads with it and
// consumers overwhelmingly want "everything that happened to this
// card". Ids pass through [sanitiseID] so a caller-supplied id cannot
// fragment the subject or forge a wildcard segment.
const cardSubjectPrefix = "kanban.card."

// subjectFor derives the wire subject from an event kind and card id.
// The kind constants already end in the transition name, so the suffix
// is the kind's last segment.
func subjectFor(kind, cardID string) event.Subject {
	suffix := kind[len("kanban.card."):]
	return event.Subject(fmt.Sprintf("%s%s.%s",
		cardSubjectPrefix, sanitiseID(cardID), suffix))
}

// PatternCard matches every event for one card.
func PatternCard(cardID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.>", cardSubjectPrefix, sanitiseID(cardID)))
}

// PatternAll matches every event from a board.
func PatternAll() event.Pattern { return event.Pattern("kanban.card.>") }

// sanitiseID neutralises characters that would corrupt a Subject.
// Mirrors sdk/graph/executor/subjects.go; each package keeps a private
// copy so neither has to depend on the other.
func sanitiseID(id string) string {
	if id == "" {
		return "_"
	}
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		switch id[i] {
		case '.', '*', '>':
			out = append(out, '_')
		default:
			out = append(out, id[i])
		}
	}
	return string(out)
}
