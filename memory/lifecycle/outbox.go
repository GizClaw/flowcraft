package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const outboxSchemaVersion = 1

type StateEvent string

const (
	EventFactPublished StateEvent = "fact_published"
	EventFactMerged    StateEvent = "fact_merged"
)

type Task struct {
	ID             string          `json:"id"`
	Scope          sdkmemory.Scope `json:"scope"`
	FactID         string          `json:"fact_id"`
	ConversationID string          `json:"conversation_id"`
	StateEvent     StateEvent      `json:"state_event"`
	PublicationID  string          `json:"publication_id"`
	RevisionDigest string          `json:"revision_digest"`
	PolicyDigest   string          `json:"policy_digest"`
	Branch         string          `json:"branch"`
	CreatedAt      time.Time       `json:"created_at"`
}

type Lease struct {
	Task      Task      `json:"task"`
	Token     string    `json:"token"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type TaskStatus string

const (
	TaskLeased    TaskStatus = "leased"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
)

type taskEvent struct {
	SchemaVersion int        `json:"schema_version"`
	Sequence      uint64     `json:"sequence"`
	TaskID        string     `json:"task_id"`
	Status        TaskStatus `json:"status"`
	Token         string     `json:"token"`
	Owner         string     `json:"owner,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	Time          time.Time  `json:"time"`
}

type taskEnvelope struct {
	SchemaVersion int  `json:"schema_version"`
	Task          Task `json:"task"`
}

var outboxProcessMu sync.Mutex

type WorkspaceOutbox struct {
	ws     workspace.Workspace
	clock  Clock
	notify chan struct{}
}

func NewWorkspaceOutbox(ws workspace.Workspace, clock Clock) (*WorkspaceOutbox, error) {
	if ws == nil {
		return nil, errors.New("memory lifecycle outbox: workspace is required")
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &WorkspaceOutbox{ws: ws, clock: clock, notify: make(chan struct{}, 1)}, nil
}

func (outbox *WorkspaceOutbox) Enqueue(ctx context.Context, task Task) (Task, error) {
	if err := validateTask(task, false); err != nil {
		return Task{}, err
	}
	if task.ID == "" {
		payload, _ := json.Marshal([]any{task.Scope, task.ConversationID, task.FactID, task.StateEvent,
			task.PublicationID, task.RevisionDigest, task.PolicyDigest, task.Branch})
		task.ID = digest("lifecycle-task", payload)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = outbox.clock.Now().UTC()
	}
	if err := validateTask(task, true); err != nil {
		return Task{}, err
	}
	data, err := json.Marshal(taskEnvelope{SchemaVersion: outboxSchemaVersion, Task: task})
	if err != nil {
		return Task{}, err
	}
	outboxProcessMu.Lock()
	defer outboxProcessMu.Unlock()
	target := outbox.taskPath(task.ID)
	existing, err := outbox.ws.Read(ctx, target)
	if err == nil {
		var stored taskEnvelope
		if decodeErr := strictOutboxDecode(existing, &stored); decodeErr != nil {
			return Task{}, decodeErr
		}
		if stored.SchemaVersion != outboxSchemaVersion || !sameTaskIdentity(stored.Task, task) {
			return Task{}, errdefs.Conflictf("memory lifecycle outbox: task %q conflicts", task.ID)
		}
		return stored.Task, nil
	}
	if !errdefs.IsNotFound(err) {
		return Task{}, err
	}
	if err := workspace.AtomicWrite(ctx, outbox.ws, target, data); err != nil {
		return Task{}, fmt.Errorf("memory lifecycle outbox: append task: %w", err)
	}
	outbox.signal()
	return task, nil
}

func sameTaskIdentity(left, right Task) bool {
	return left.ID == right.ID && left.Scope == right.Scope && left.ConversationID == right.ConversationID &&
		left.FactID == right.FactID && left.StateEvent == right.StateEvent &&
		left.PublicationID == right.PublicationID && left.RevisionDigest == right.RevisionDigest &&
		left.PolicyDigest == right.PolicyDigest && left.Branch == right.Branch
}

func (outbox *WorkspaceOutbox) signal() {
	select {
	case outbox.notify <- struct{}{}:
	default:
	}
}

func (outbox *WorkspaceOutbox) Notifications() <-chan struct{} { return outbox.notify }

func (outbox *WorkspaceOutbox) LeaseNext(ctx context.Context, scope sdkmemory.Scope, owner string, ttl time.Duration) (Lease, bool, error) {
	if err := scope.Validate(); err != nil {
		return Lease{}, false, err
	}
	if strings.TrimSpace(owner) == "" || ttl <= 0 {
		return Lease{}, false, errors.New("memory lifecycle outbox: owner and positive ttl are required")
	}
	outboxProcessMu.Lock()
	defer outboxProcessMu.Unlock()
	tasks, err := outbox.tasks(ctx)
	if err != nil {
		return Lease{}, false, err
	}
	now := outbox.clock.Now().UTC()
	for _, task := range tasks {
		if task.Scope != scope {
			continue
		}
		latest, found, err := outbox.latestEvent(ctx, task.ID)
		if err != nil {
			return Lease{}, false, err
		}
		if found && latest.Status == TaskCompleted {
			continue
		}
		if found && latest.Status == TaskLeased && latest.ExpiresAt.After(now) {
			continue
		}
		token, err := leaseToken()
		if err != nil {
			return Lease{}, false, err
		}
		lease := Lease{Task: task, Token: token, Owner: owner, ExpiresAt: now.Add(ttl)}
		event := taskEvent{SchemaVersion: outboxSchemaVersion, TaskID: task.ID, Status: TaskLeased,
			Token: token, Owner: owner, ExpiresAt: lease.ExpiresAt, Time: now}
		if err := outbox.appendTaskEvent(ctx, event); err != nil {
			return Lease{}, false, err
		}
		return lease, true, nil
	}
	return Lease{}, false, nil
}

func (outbox *WorkspaceOutbox) Complete(ctx context.Context, taskID, token string) error {
	return outbox.checkpoint(ctx, taskID, token, TaskCompleted, "")
}

// Renew extends an active lease by appending a new lease event. Only the
// latest, unexpired token may renew.
func (outbox *WorkspaceOutbox) Renew(ctx context.Context, taskID, token string, ttl time.Duration) (time.Time, error) {
	if ttl <= 0 {
		return time.Time{}, errors.New("memory lifecycle outbox: positive renewal ttl is required")
	}
	outboxProcessMu.Lock()
	defer outboxProcessMu.Unlock()
	latest, found, err := outbox.latestEvent(ctx, taskID)
	if err != nil {
		return time.Time{}, err
	}
	now := outbox.clock.Now().UTC()
	if !found || latest.Status != TaskLeased || latest.Token != token || !latest.ExpiresAt.After(now) {
		return time.Time{}, errdefs.Conflictf("memory lifecycle outbox: stale lease token")
	}
	expiresAt := now.Add(ttl)
	if err := outbox.appendTaskEvent(ctx, taskEvent{
		SchemaVersion: outboxSchemaVersion, TaskID: taskID, Status: TaskLeased,
		Token: token, Owner: latest.Owner, ExpiresAt: expiresAt, Time: now,
	}); err != nil {
		return time.Time{}, err
	}
	return expiresAt, nil
}

func (outbox *WorkspaceOutbox) Fail(ctx context.Context, taskID, token string, cause error) error {
	message := "failed"
	if cause != nil {
		message = cause.Error()
	}
	return outbox.checkpoint(ctx, taskID, token, TaskFailed, message)
}

func (outbox *WorkspaceOutbox) checkpoint(ctx context.Context, taskID, token string, status TaskStatus, message string) error {
	outboxProcessMu.Lock()
	defer outboxProcessMu.Unlock()
	latest, found, err := outbox.latestEvent(ctx, taskID)
	if err != nil {
		return err
	}
	now := outbox.clock.Now().UTC()
	if !found || latest.Status != TaskLeased || latest.Token != token || !latest.ExpiresAt.After(now) {
		return errdefs.Conflictf("memory lifecycle outbox: stale lease token")
	}
	return outbox.appendTaskEvent(ctx, taskEvent{
		SchemaVersion: outboxSchemaVersion, TaskID: taskID, Status: status,
		Token: token, Error: message, Time: now,
	})
}

func (outbox *WorkspaceOutbox) tasks(ctx context.Context) ([]Task, error) {
	entries, err := outbox.ws.List(ctx, path.Join("outbox", "memory-lifecycle", "v1", "tasks"))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return []Task{}, nil
		}
		return nil, err
	}
	result := make([]Task, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := outbox.ws.Read(ctx, path.Join("outbox", "memory-lifecycle", "v1", "tasks", entry.Name()))
		if err != nil {
			return nil, err
		}
		var value taskEnvelope
		if err := strictOutboxDecode(data, &value); err != nil {
			return nil, err
		}
		if value.SchemaVersion != outboxSchemaVersion {
			return nil, errors.New("memory lifecycle outbox: unsupported task schema")
		}
		result = append(result, value.Task)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func (outbox *WorkspaceOutbox) latestEvent(ctx context.Context, taskID string) (taskEvent, bool, error) {
	entries, err := outbox.ws.List(ctx, outbox.eventsDir(taskID))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return taskEvent{}, false, nil
		}
		return taskEvent{}, false, err
	}
	var events []taskEvent
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := outbox.ws.Read(ctx, path.Join(outbox.eventsDir(taskID), entry.Name()))
		if err != nil {
			return taskEvent{}, false, err
		}
		var event taskEvent
		if err := strictOutboxDecode(data, &event); err != nil {
			return taskEvent{}, false, err
		}
		if event.SchemaVersion != outboxSchemaVersion || event.TaskID != taskID {
			return taskEvent{}, false, errors.New("memory lifecycle outbox: corrupt task event")
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return taskEvent{}, false, nil
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Sequence != events[j].Sequence {
			return events[i].Sequence < events[j].Sequence
		}
		return events[i].Token < events[j].Token
	})
	return events[len(events)-1], true, nil
}

func (outbox *WorkspaceOutbox) appendTaskEvent(ctx context.Context, event taskEvent) error {
	if event.Sequence == 0 {
		entries, err := outbox.ws.List(ctx, outbox.eventsDir(event.TaskID))
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, readErr := outbox.ws.Read(ctx, path.Join(outbox.eventsDir(event.TaskID), entry.Name()))
			if readErr != nil {
				return readErr
			}
			var prior taskEvent
			if decodeErr := strictOutboxDecode(data, &prior); decodeErr != nil {
				return decodeErr
			}
			if prior.Sequence >= event.Sequence {
				event.Sequence = prior.Sequence + 1
			}
		}
		if event.Sequence == 0 {
			event.Sequence = 1
		}
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s-%s.json", event.Sequence, event.Status, encodeOutbox(event.Token))
	target := path.Join(outbox.eventsDir(event.TaskID), name)
	existing, err := outbox.ws.Read(ctx, target)
	if err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return errdefs.Conflictf("memory lifecycle outbox: event conflict")
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	return workspace.AtomicWrite(ctx, outbox.ws, target, data)
}

func validateTask(task Task, complete bool) error {
	if err := task.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(task.FactID) == "" || strings.TrimSpace(task.PublicationID) == "" ||
		strings.TrimSpace(task.RevisionDigest) == "" || strings.TrimSpace(task.PolicyDigest) == "" ||
		strings.TrimSpace(task.Branch) == "" {
		return errors.New("memory lifecycle outbox: fact_id, publication_id, revision_digest, policy_digest, and branch are required")
	}
	if task.StateEvent != EventFactPublished && task.StateEvent != EventFactMerged {
		return errors.New("memory lifecycle outbox: invalid state event")
	}
	if complete && (task.ID == "" || task.CreatedAt.IsZero()) {
		return errors.New("memory lifecycle outbox: id and created_at are required")
	}
	return nil
}

func (outbox *WorkspaceOutbox) taskPath(id string) string {
	return path.Join("outbox", "memory-lifecycle", "v1", "tasks", encodeOutbox(id)+".json")
}
func (outbox *WorkspaceOutbox) eventsDir(id string) string {
	return path.Join("outbox", "memory-lifecycle", "v1", "events", encodeOutbox(id))
}
func encodeOutbox(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}
func leaseToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
func strictOutboxDecode(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}
