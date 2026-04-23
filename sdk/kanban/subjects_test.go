package kanban

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/event"
)

func TestKanbanSubjects_FormatAndValidate(t *testing.T) {
	t.Parallel()

	const (
		cardID     = "card-abc"
		scheduleID = "sched-42"
	)
	cases := []struct {
		name string
		got  event.Subject
		want event.Subject
	}{
		{"task.submitted", subjTaskSubmitted(cardID), "kanban.card.card-abc.task.submitted"},
		{"task.claimed", subjTaskClaimed(cardID), "kanban.card.card-abc.task.claimed"},
		{"task.completed", subjTaskCompleted(cardID), "kanban.card.card-abc.task.completed"},
		{"task.failed", subjTaskFailed(cardID), "kanban.card.card-abc.task.failed"},
		{"callback.start", subjCallbackStart(cardID), "kanban.card.card-abc.callback.start"},
		{"callback.done", subjCallbackDone(cardID), "kanban.card.card-abc.callback.done"},
		{"cron.created", subjCronCreated(scheduleID), "kanban.cron.sched-42.rule.created"},
		{"cron.fired", subjCronFired(scheduleID), "kanban.cron.sched-42.rule.fired"},
		{"cron.disabled", subjCronDisabled(scheduleID), "kanban.cron.sched-42.rule.disabled"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if c.got != c.want {
				t.Fatalf("subject = %q, want %q", c.got, c.want)
			}
			if err := c.got.Validate(); err != nil {
				t.Fatalf("Validate(%q): %v", c.got, err)
			}
		})
	}
}

func TestKanbanPatterns_MatchOwnSubjects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pattern event.Pattern
		matches []event.Subject
		misses  []event.Subject
	}{
		{
			name:    "PatternCard",
			pattern: PatternCard("c1"),
			matches: []event.Subject{
				subjTaskSubmitted("c1"),
				subjCallbackStart("c1"),
			},
			misses: []event.Subject{
				subjTaskSubmitted("c2"),
				subjCronFired("c1"),
			},
		},
		{
			name:    "PatternAllCards",
			pattern: PatternAllCards(),
			matches: []event.Subject{
				subjTaskSubmitted("anything"),
				subjCallbackDone("xyz"),
			},
			misses: []event.Subject{
				subjCronCreated("s1"),
			},
		},
		{
			name:    "PatternCronRule",
			pattern: PatternCronRule("s1"),
			matches: []event.Subject{
				subjCronCreated("s1"),
				subjCronFired("s1"),
				subjCronDisabled("s1"),
			},
			misses: []event.Subject{
				subjCronCreated("s2"),
				subjTaskSubmitted("s1"),
			},
		},
		{
			name:    "PatternAll",
			pattern: PatternAll(),
			matches: []event.Subject{
				subjTaskSubmitted("c1"),
				subjCronFired("s1"),
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := c.pattern.Validate(); err != nil {
				t.Fatalf("Validate(%q): %v", c.pattern, err)
			}
			for _, s := range c.matches {
				if !c.pattern.Matches(s) {
					t.Errorf("pattern %q should match %q", c.pattern, s)
				}
			}
			for _, s := range c.misses {
				if c.pattern.Matches(s) {
					t.Errorf("pattern %q should NOT match %q", c.pattern, s)
				}
			}
		})
	}
}

func TestSanitiseID_EscapesSubjectMetachars(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"abc", "abc"},
		{"", "_"},
		{"a.b", "a_b"},
		{"a*b", "a_b"},
		{"a>b", "a_b"},
		{"a.b*c>d", "a_b_c_d"},
	}
	for _, c := range cases {
		if got := sanitiseID(c.in); got != c.want {
			t.Errorf("sanitiseID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitiseID_PreservesSubjectStructure(t *testing.T) {
	t.Parallel()

	// A user-supplied ID containing dots must not turn one segment into
	// many: subjTaskSubmitted("a.b") must still be a 5-segment subject so
	// the convention "kanban.card.<id>.task.submitted" stays well-formed.
	got := subjTaskSubmitted("a.b.c")
	const want event.Subject = "kanban.card.a_b_c.task.submitted"
	if got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate(%q): %v", got, err)
	}
}
