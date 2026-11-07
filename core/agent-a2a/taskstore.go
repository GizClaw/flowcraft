package a2a

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// TaskStore serves A2A task queries from the agent's checkpoint store.
//
// Without it the server keeps a second, in-memory record of the same task: a
// restart loses what tasks/get answers while the checkpoint still holds it, and
// the two records can disagree about state.
//
// One writer owns the projection. The loop advances a task's state, round and
// history; this adapter appends the events the server hands it and never decides
// a state of its own. A store adapter that also advanced state would fight the
// loop over the same row.
type TaskStore struct {
	store agent.Store
}

var _ taskstore.Store = (*TaskStore)(nil)

// NewTaskStore wraps an agent store.
func NewTaskStore(store agent.Store) (*TaskStore, error) {
	if store == nil {
		return nil, errdefs.Validationf("a2a: a checkpoint store is required")
	}
	return &TaskStore{store: store}, nil
}

// Create stores a task the server just created.
func (s *TaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil || task.ID == "" {
		return 0, errdefs.Validationf("a2a: a task without an id cannot be stored")
	}
	if _, err := s.store.Load(ctx, string(task.ID)); err == nil {
		return 0, taskstore.ErrTaskAlreadyExists
	} else if !errors.Is(err, agent.ErrNoCheckpoint) {
		return 0, err
	}
	core, err := fromA2ATask(task)
	if err != nil {
		return 0, err
	}
	checkpoint := agent.Checkpoint{Schema: agent.CheckpointSchema, Task: core}
	// The history the task arrives with is the first batch of events, so history
	// has exactly one source everywhere: the event log.
	events, err := messageEvents(core.History)
	if err != nil {
		return 0, err
	}
	if err := s.store.Save(ctx, checkpoint, events); err != nil {
		return 0, err
	}
	return s.version(ctx, string(task.ID))
}

// Update appends the event the server produced and leaves the projection to the
// loop.
func (s *TaskStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if update == nil || update.Task == nil || update.Task.ID == "" {
		return 0, errdefs.Validationf("a2a: an update without a task cannot be applied")
	}
	taskID := string(update.Task.ID)
	current, err := s.store.Load(ctx, taskID)
	if errors.Is(err, agent.ErrNoCheckpoint) {
		return 0, a2a.ErrTaskNotFound
	}
	if err != nil {
		return 0, err
	}
	if update.PrevVersion != taskstore.TaskVersionMissing &&
		taskstore.TaskVersion(current.Version) != update.PrevVersion {
		return 0, taskstore.ErrConcurrentModification
	}
	events, err := eventsOf(update.Event)
	if err != nil {
		return 0, err
	}
	if err := s.store.Save(ctx, current, events); err != nil {
		return 0, err
	}
	return s.version(ctx, taskID)
}

// Get returns the stored task with its history from the event log.
func (s *TaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	checkpoint, err := s.store.Load(ctx, string(taskID))
	if errors.Is(err, agent.ErrNoCheckpoint) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	events, err := s.store.Events(ctx, string(taskID))
	if err != nil {
		return nil, err
	}
	task, err := toA2ATask(checkpoint, events)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(checkpoint.Version)}, nil
}

// List answers the tasks query, filtered by context when asked.
func (s *TaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	summaries, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	contextID := ""
	if req != nil {
		contextID = req.ContextID
	}
	tasks := make([]*a2a.Task, 0, len(summaries))
	for _, summary := range summaries {
		if contextID != "" && summary.ContextID != contextID {
			continue
		}
		tasks = append(tasks, &a2a.Task{
			ID:        a2a.TaskID(summary.ID),
			ContextID: summary.ContextID,
			Status:    a2a.TaskStatus{State: stateOf(summary.State)},
		})
	}
	return &a2a.ListTasksResponse{Tasks: tasks}, nil
}

func (s *TaskStore) version(ctx context.Context, taskID string) (taskstore.TaskVersion, error) {
	checkpoint, err := s.store.Load(ctx, taskID)
	if err != nil {
		return 0, err
	}
	return taskstore.TaskVersion(checkpoint.Version), nil
}

// eventsOf records what the server appended. Only messages have a core event
// form; status events are the loop's to produce.
func eventsOf(event a2a.Event) ([]agent.EventRecord, error) {
	msg, ok := event.(*a2a.Message)
	if !ok || msg == nil {
		return nil, nil
	}
	core, err := fromA2AMessage(msg)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(core)
	if err != nil {
		return nil, errdefs.Validationf("a2a: encode message event: %w", err)
	}
	return []agent.EventRecord{{Kind: "message", Payload: body}}, nil
}

// messageEvents records a batch of messages as events.
func messageEvents(messages []message.Message) ([]agent.EventRecord, error) {
	events := make([]agent.EventRecord, 0, len(messages))
	for _, msg := range messages {
		body, err := json.Marshal(msg)
		if err != nil {
			return nil, errdefs.Validationf("a2a: encode message event: %w", err)
		}
		events = append(events, agent.EventRecord{Kind: "message", Payload: body})
	}
	return events, nil
}

// fromA2ATask converts an incoming task into the core shape.
func fromA2ATask(task *a2a.Task) (agent.Task, error) {
	history := make([]message.Message, 0, len(task.History))
	for _, msg := range task.History {
		converted, err := fromA2AMessage(msg)
		if err != nil {
			return agent.Task{}, err
		}
		history = append(history, converted)
	}
	return agent.Task{
		ID:        string(task.ID),
		ContextID: task.ContextID,
		State:     stateFromA2A(task.Status.State),
		History:   history,
	}, nil
}

// toA2ATask renders a stored task; its history is the message events.
func toA2ATask(checkpoint agent.Checkpoint, events []agent.EventRecord) (*a2a.Task, error) {
	history := make([]*a2a.Message, 0, len(events))
	for _, event := range events {
		if event.Kind != "message" {
			continue
		}
		var core message.Message
		if err := json.Unmarshal(event.Payload, &core); err != nil {
			return nil, errdefs.Validationf("a2a: decode message event: %w", err)
		}
		converted, err := toA2AMessage(core)
		if err != nil {
			return nil, err
		}
		history = append(history, converted)
	}
	return &a2a.Task{
		ID:        a2a.TaskID(checkpoint.Task.ID),
		ContextID: checkpoint.Task.ContextID,
		Status:    a2a.TaskStatus{State: stateOf(checkpoint.Task.State)},
		History:   history,
	}, nil
}

// toA2AMessage renders one core message for the wire.
func toA2AMessage(msg message.Message) (*a2a.Message, error) {
	role := a2a.MessageRoleAgent
	if msg.Role == message.RoleUser {
		role = a2a.MessageRoleUser
	}
	parts := make([]*a2a.Part, 0, len(msg.Content.Parts))
	for _, part := range msg.Content.Parts {
		switch typed := part.(type) {
		case message.TextPart:
			parts = append(parts, a2a.NewTextPart(typed.Text))
		case message.DataPart:
			parts = append(parts, a2a.NewDataPart(typed.Value))
		case message.FilePart:
			parts = append(parts, a2a.NewFileURLPart(a2a.URL(typed.URI), typed.MediaType))
		default:
			return nil, errdefs.Validationf("a2a: part kind %q has no wire form", part.Kind())
		}
	}
	return a2a.NewMessage(role, parts...), nil
}

// stateFromA2A translates the protocol's spelling back into the internal one.
func stateFromA2A(state a2a.TaskState) agent.TaskState {
	switch state {
	case a2a.TaskStateSubmitted:
		return agent.StateSubmitted
	case a2a.TaskStateWorking:
		return agent.StateWorking
	case a2a.TaskStateInputRequired:
		return agent.StateInputRequired
	case a2a.TaskStateCompleted:
		return agent.StateCompleted
	case a2a.TaskStateCanceled:
		return agent.StateCanceled
	case a2a.TaskStateRejected:
		return agent.StateRejected
	case a2a.TaskStateAuthRequired:
		return agent.StateAuthRequired
	default:
		return agent.StateFailed
	}
}
