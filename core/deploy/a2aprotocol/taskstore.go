package a2aprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// taskStore serves A2A task queries from the agent's checkpoint store.
//
// Without it the server keeps a second, in-memory record of the same task: a
// restart loses what tasks/get answers while the checkpoint still holds it, and
// the two records can disagree about state.
//
// One writer owns the projection. The loop advances a task's state, round and
// history; this adapter appends the events the server hands it and never decides
// a state of its own. A store adapter that also advanced state would fight the
// loop over the same row.
type taskStore struct {
	store agent.Store
	// fingerprint is the deployment identity the agent resumes under; a task row
	// without it could never be restored.
	fingerprint agent.Fingerprint
}

var _ taskstore.Store = (*taskStore)(nil)

// newTaskStore wraps an agent store.
func newTaskStore(store agent.Store, fingerprint agent.Fingerprint) *taskStore {
	return &taskStore{store: store, fingerprint: fingerprint}
}

// Create stores a task the server just created.
func (s *taskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil || task.ID == "" {
		return 0, errdefs.Validationf("a2a: a task without an id cannot be stored")
	}
	// The server creates the task before it executes, and the executor's first
	// event carries the same task back. Re-creating it must be idempotent, or the
	// server rejects its own reservation; a genuinely different task under the
	// same id is the only case worth refusing.
	if existing, err := s.store.Load(ctx, string(task.ID)); err == nil {
		if existing.Task.SessionID != task.ContextID {
			return 0, taskstore.ErrTaskAlreadyExists
		}
		return taskstore.TaskVersion(existing.ServerVersion), nil
	} else if !errors.Is(err, agent.ErrNoCheckpoint) {
		return 0, err
	}
	core, err := fromA2ATask(task)
	if err != nil {
		return 0, err
	}
	checkpoint := agent.Checkpoint{
		Schema:        agent.CheckpointSchema,
		Fingerprint:   s.fingerprint,
		ServerVersion: 1,
		Task:          core,
	}
	// The history the task arrives with is the first batch of events, so history
	// has exactly one source everywhere: the event log.
	events, err := messageEvents(core.History)
	if err != nil {
		return 0, err
	}
	if _, _, err := s.store.Save(ctx, checkpoint, events); err != nil {
		return 0, err
	}
	return s.version(ctx, string(task.ID))
}

// Update records one event the server produced.
//
// The engine owns the task state; the adapter owns the server's token. A status
// or artifact event carries nothing durable — the engine has already checkpointed
// it — so the projection is left alone and only the token advances. A message
// event is task history and a terminal state is the server's decision, and those
// two are written.
func (s *taskStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if update == nil || update.Task == nil || update.Task.ID == "" {
		return 0, errdefs.Validationf("a2a: an update without a task cannot be applied")
	}
	taskID := string(update.Task.ID)
	events, err := eventsOf(update.Event)
	if err != nil {
		return 0, err
	}
	terminal := isTerminal(update.Task.Status.State)
	if !terminal && len(events) == 0 {
		// The token advances in one statement; rewriting the projection here
		// would fight the engine's checkpoint for the same row on every chunk.
		next := update.PrevVersion
		if next == taskstore.TaskVersionMissing {
			current, loadErr := s.store.Load(ctx, taskID)
			if errors.Is(loadErr, agent.ErrNoCheckpoint) {
				return 0, a2a.ErrTaskNotFound
			}
			if loadErr != nil {
				return 0, loadErr
			}
			next = taskstore.TaskVersion(current.ServerVersion)
		}
		version := int64(next) + 1
		if err := s.store.SetServerVersion(ctx, taskID, version); err != nil {
			if errors.Is(err, agent.ErrNoCheckpoint) {
				return 0, a2a.ErrTaskNotFound
			}
			return 0, err
		}
		return taskstore.TaskVersion(version), nil
	}
	current, err := s.store.Load(ctx, taskID)
	if errors.Is(err, agent.ErrNoCheckpoint) {
		return 0, a2a.ErrTaskNotFound
	}
	if err != nil {
		return 0, err
	}
	// The adapter owns this counter; the loop owns the other one. Sharing a
	// single version would make the loop's round-boundary save invalidate the
	// server's token and fail every tool-using task.
	if update.PrevVersion != taskstore.TaskVersionMissing &&
		taskstore.TaskVersion(current.ServerVersion) != update.PrevVersion {
		return 0, taskstore.ErrConcurrentModification
	}
	current.ServerVersion++
	if terminal {
		// A cancel or a failure is decided by the server, not by the loop, so the
		// projection has to take it from here.
		current.Task.State = wire.FromState(update.Task.Status.State)
	}
	if _, _, err := s.store.Save(ctx, current, events); err != nil {
		return 0, err
	}
	return taskstore.TaskVersion(current.ServerVersion), nil
}

// Get returns the stored task with its history from the event log.
func (s *taskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
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
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(checkpoint.ServerVersion)}, nil
}

// List answers the tasks query, filtered by context when asked.
// List answers a filtered page. The store returns every summary; the adapter
// applies the protocol's filters and pagination, because a page is a wire
// concern and the store's job is the newest-first order.
const (
	listDefaultPageSize = 50
	listMaxPageSize     = 100
)

func (s *taskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	summaries, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var filter a2a.ListTasksRequest
	if req != nil {
		filter = *req
	}
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = listDefaultPageSize
	}
	if pageSize > listMaxPageSize {
		pageSize = listMaxPageSize
	}
	offset, err := pageOffset(filter.PageToken)
	if err != nil {
		return nil, err
	}
	matching := make([]agent.TaskSummary, 0, len(summaries))
	for _, summary := range summaries {
		if filter.ContextID != "" && summary.SessionID != filter.ContextID {
			continue
		}
		if filter.Status != "" && wire.State(summary.State) != filter.Status {
			continue
		}
		matching = append(matching, summary)
	}
	start := offset
	if start > len(matching) {
		start = len(matching)
	}
	end := start + pageSize
	if end > len(matching) {
		end = len(matching)
	}
	tasks := make([]*a2a.Task, 0, end-start)
	for _, summary := range matching[start:end] {
		task := &a2a.Task{
			ID:        a2a.TaskID(summary.ID),
			ContextID: summary.SessionID,
			Status:    a2a.TaskStatus{State: wire.State(summary.State)},
		}
		if filter.HistoryLength != nil && *filter.HistoryLength > 0 {
			events, err := s.store.Events(ctx, summary.ID)
			if err != nil {
				return nil, err
			}
			history, err := historyOf(events, *filter.HistoryLength)
			if err != nil {
				return nil, err
			}
			task.History = history
		}
		tasks = append(tasks, task)
	}
	response := &a2a.ListTasksResponse{
		Tasks:     tasks,
		PageSize:  pageSize,
		TotalSize: len(matching),
	}
	if end < len(matching) {
		response.NextPageToken = strconv.Itoa(end)
	}
	return response, nil
}

// pageOffset reads the token the previous page left. The token is the offset
// into the filtered list; anything else is a caller error, not an empty page.
func pageOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(token)
	if err != nil || offset < 0 {
		return 0, errdefs.Validationf("a2a: page token %q is not an offset", token)
	}
	return offset, nil
}

func (s *taskStore) version(ctx context.Context, taskID string) (taskstore.TaskVersion, error) {
	checkpoint, err := s.store.Load(ctx, taskID)
	if err != nil {
		return 0, err
	}
	return taskstore.TaskVersion(checkpoint.ServerVersion), nil
}

// eventsOf records what the server appended. Only messages have a core event
// form; status events are the loop's to produce.
func eventsOf(event a2a.Event) ([]agent.EventRecord, error) {
	msg, ok := event.(*a2a.Message)
	if !ok || msg == nil {
		return nil, nil
	}
	core, err := wire.FromMessage(msg)
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
		converted, err := wire.FromMessage(msg)
		if err != nil {
			return agent.Task{}, err
		}
		history = append(history, converted)
	}
	return agent.Task{
		ID:        string(task.ID),
		SessionID: task.ContextID,
		State:     wire.FromState(task.Status.State),
		History:   history,
	}, nil
}

// toA2ATask renders a stored task; its history is the message events.
func toA2ATask(checkpoint agent.Checkpoint, events []agent.EventRecord) (*a2a.Task, error) {
	history, err := historyOf(events, 0)
	if err != nil {
		return nil, err
	}
	return &a2a.Task{
		ID:        a2a.TaskID(checkpoint.Task.ID),
		ContextID: checkpoint.Task.SessionID,
		Status:    a2a.TaskStatus{State: wire.State(checkpoint.Task.State)},
		History:   history,
	}, nil
}

// historyOf renders the message events of a task as wire history, keeping at
// most limit messages from the end when limit is positive.
func historyOf(events []agent.EventRecord, limit int) ([]*a2a.Message, error) {
	history := make([]*a2a.Message, 0, len(events))
	for _, event := range events {
		if event.Kind != "message" {
			continue
		}
		var core message.Message
		if err := json.Unmarshal(event.Payload, &core); err != nil {
			return nil, errdefs.Validationf("a2a: decode message event: %w", err)
		}
		converted, err := wire.Message(core)
		if err != nil {
			// The loop persists its own working (tool calls, results, reasoning)
			// in the same log; those parts have no wire form. Skipping them keeps
			// the transcript readable instead of failing the whole task: an
			// internal bookkeeping record must never break tasks/get.
			continue
		}
		history = append(history, converted)
	}
	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}
	return history, nil
}

// isTerminal reports whether a wire state ends a task.
func isTerminal(state a2a.TaskState) bool {
	switch state {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	default:
		return false
	}
}
