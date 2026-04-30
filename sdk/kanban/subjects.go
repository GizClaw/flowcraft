package kanban

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// Subject convention emitted by the Kanban Board (sdk-internal; no
// relation to any flowcraft business schema):
//
//	kanban.card.<cardID>.task.submitted
//	kanban.card.<cardID>.task.claimed
//	kanban.card.<cardID>.task.completed
//	kanban.card.<cardID>.task.failed
//	kanban.card.<cardID>.task.cancelled
//	kanban.card.<cardID>.callback.start
//	kanban.card.<cardID>.callback.done
//	kanban.cron.<scheduleID>.rule.created
//	kanban.cron.<scheduleID>.rule.fired
//	kanban.cron.<scheduleID>.rule.disabled
//
// Choice of partition key:
//   - card_id for task / callback events: every payload already carries
//     CardID as its first field, and consumers overwhelmingly want
//     "tell me everything that happened to this card";
//   - schedule_id for cron events: cron rules outlive any single card
//     they spawn, and the natural subscription scope is the rule itself.
//
// IDs go through sanitiseID (replaces '.', '*', '>' with '_') so a
// caller-supplied id cannot fragment the subject or accidentally form a
// wildcard segment.

const (
	kanbanCardSubjectPrefix = "kanban.card."
	kanbanCronSubjectPrefix = "kanban.cron."
)

// Subject helpers — task domain.

func subjTaskSubmitted(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.task.submitted", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

func subjTaskClaimed(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.task.claimed", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

func subjTaskCompleted(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.task.completed", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

func subjTaskFailed(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.task.failed", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

func subjTaskCancelled(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.task.cancelled", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

// Subject helpers — callback domain.

func subjCallbackStart(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.callback.start", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

func subjCallbackDone(cardID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.callback.done", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

// Subject helpers — cron domain.

func subjCronCreated(scheduleID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.rule.created", kanbanCronSubjectPrefix, sanitiseID(scheduleID)))
}

func subjCronFired(scheduleID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.rule.fired", kanbanCronSubjectPrefix, sanitiseID(scheduleID)))
}

func subjCronDisabled(scheduleID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.rule.disabled", kanbanCronSubjectPrefix, sanitiseID(scheduleID)))
}

// Convenience patterns for downstream subscribers. They mirror the most
// common slices a UI / projector wants to consume.

// PatternCard returns "kanban.card.<cardID>.>" — every event for one card.
func PatternCard(cardID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.>", kanbanCardSubjectPrefix, sanitiseID(cardID)))
}

// PatternAllCards returns "kanban.card.>" — every card-scoped event from
// any card on this board.
func PatternAllCards() event.Pattern { return event.Pattern("kanban.card.>") }

// PatternCronRule returns "kanban.cron.<scheduleID>.>" — every event for
// one cron rule.
func PatternCronRule(scheduleID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.>", kanbanCronSubjectPrefix, sanitiseID(scheduleID)))
}

// PatternAllCron returns "kanban.cron.>" — every cron-scoped event.
func PatternAllCron() event.Pattern { return event.Pattern("kanban.cron.>") }

// PatternAll returns "kanban.>" — every Kanban event.
func PatternAll() event.Pattern { return event.Pattern("kanban.>") }

// sanitiseID escapes characters that would corrupt a Subject. See
// sdk/graph/executor/subjects.go for the rationale; the implementation
// is intentionally identical and kept private to each package so neither
// has to depend on the other.
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
