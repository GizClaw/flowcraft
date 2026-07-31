package kanban_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/kanban"
)

func TestTaskContextIncludesRequestAndIntent(t *testing.T) {
	k := newBoard(t)
	card, err := k.Submit(context.Background(), kanban.Task{
		TargetAgentID: "researcher",
		Query:         "find the latest figures",
		UserQuery:     "how did we do last quarter",
		DispatchNote:  "delegated because I lack data access",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got := card.TaskContext()
	for _, want := range []string{
		card.ID,
		"find the latest figures",
		"how did we do last quarter",
		"delegated because I lack data access",
		"researcher",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("TaskContext missing %q:\n%s", want, got)
		}
	}
}

func TestTaskContextTellsTheReaderWhatToDo(t *testing.T) {
	tests := []struct {
		name  string
		drive func(*kanban.Kanban, string)
		want  string
	}{
		{"pending", func(*kanban.Kanban, string) {}, "do not resubmit"},
		{"claimed", func(k *kanban.Kanban, id string) {
			k.Claim(id, "worker-1")
		}, "do not resubmit"},
		{"suspended", func(k *kanban.Kanban, id string) {
			k.Claim(id, "worker-1")
			k.Suspend(id, "ckpt")
		}, "do not resubmit"},
		{"done", func(k *kanban.Kanban, id string) {
			k.Claim(id, "worker-1")
			k.Done(id, kanban.Result{Output: "the answer"})
		}, "the answer"},
		{"failed", func(k *kanban.Kanban, id string) {
			k.Claim(id, "worker-1")
			k.Fail(id, "it exploded")
		}, "it exploded"},
		{"cancelled", func(k *kanban.Kanban, id string) {
			k.Cancel(id, "no longer needed")
		}, "no longer needed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := newBoard(t)
			card := mustSubmit(t, k, "w")
			tc.drive(k, card.ID)
			fresh, _ := k.Card(card.ID)

			got := fresh.TaskContext()
			if !strings.Contains(got, tc.want) {
				t.Errorf("TaskContext(%s) missing %q:\n%s", tc.name, tc.want, got)
			}
		})
	}
}

func TestTaskContextOmitsAbsentOptionalFields(t *testing.T) {
	k := newBoard(t)
	card := mustSubmit(t, k, "w")

	got := card.TaskContext()
	if strings.Contains(got, "Original request") {
		t.Errorf("rendered an empty UserQuery section:\n%s", got)
	}
	if strings.Contains(got, "Dispatch note") {
		t.Errorf("rendered an empty DispatchNote section:\n%s", got)
	}
}
