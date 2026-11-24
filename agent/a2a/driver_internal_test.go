package a2a

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
)

// A peer may announce input-required through a status update rather than a full Task. The driver
// must learn the peer's task id from that event, or a resumed turn and a cancel become a new task
// and a no-op.
func TestStatusUpdateRemembersTheTaskID(t *testing.T) {
	driver := &driver{}
	var states []agent.TaskState
	done, err := driver.handle(&sdk.TaskStatusUpdateEvent{
		TaskID:    "task-1",
		ContextID: "ctx-1",
		Status:    sdk.TaskStatus{State: sdk.TaskStateInputRequired},
	}, func(event agent.Event, err error) bool {
		if err != nil {
			t.Fatalf("handle: %v", err)
		}
		if status, ok := event.(agent.Status); ok {
			states = append(states, status.State)
		}
		return true
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if done {
		t.Fatal("input-required must not be terminal")
	}
	driver.mu.Lock()
	taskID, contextID := driver.remote.TaskID, driver.remote.ContextID
	driver.mu.Unlock()
	if taskID != "task-1" || contextID != "ctx-1" {
		t.Fatalf("task/context = %q/%q, want task-1/ctx-1", taskID, contextID)
	}
	if len(states) != 1 || states[0] != agent.StateInputRequired {
		t.Fatalf("states = %v, want input-required", states)
	}
}

// An artifact event can arrive before the status update that names the task;
// the id must still be remembered for a later persist or cancel.
func TestArtifactUpdateRemembersTheTaskID(t *testing.T) {
	driver := &driver{}
	_, err := driver.handle(&sdk.TaskArtifactUpdateEvent{
		TaskID:    "task-2",
		ContextID: "ctx-2",
		Artifact:  &sdk.Artifact{ID: "a-1"},
	}, func(agent.Event, error) bool { return true })
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	driver.mu.Lock()
	taskID, contextID := driver.remote.TaskID, driver.remote.ContextID
	driver.mu.Unlock()
	if taskID != "task-2" || contextID != "ctx-2" {
		t.Fatalf("task/context = %q/%q, want task-2/ctx-2", taskID, contextID)
	}
}
