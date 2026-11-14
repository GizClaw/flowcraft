package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// ErrResultUnsupported reports that the engine behind a session cannot answer
// with a snapshot: it implements Runner but not ResultReader.
var ErrResultUnsupported = errors.New("agent: the engine cannot report a result")

// Result is a read-only snapshot of one Task. Content is the final assistant
// message only; a caller that needs the whole transcript reads the checkpoint
// instead.
type Result struct {
	// TaskID is the local checkpoint identity, filled by Agent.Result.
	TaskID string
	// State is where the Task stands.
	State TaskState
	// Content is the final assistant message. It is zero when the Task has no
	// final answer (failed, canceled, or waiting for input).
	Content message.Content
	// Artifacts are the references the Task produced.
	Artifacts []Artifact
	// Question is the child's request when State is StateInputRequired.
	Question *message.Content
	// Usage is optional: nil means the engine could not report it.
	Usage *usage.Usage
}

// ResultReader is an optional engine capability: a Runner that can report the
// snapshot a completed or paused Task produced. A Runner without it is still
// valid; callers get ErrResultUnsupported.
type ResultReader interface {
	Result(ctx context.Context) (Result, error)
}

// Result returns the snapshot of one session's Task.
//
// A run in flight is busy: the engine is still writing the result, so the
// caller retries after the run ends. A session that is not resident is opened
// from the store first, which is what lets a restarted process answer for a
// task it did not run.
func (a *Agent) Result(ctx context.Context, sessionID string) (Result, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Result{}, ErrSessionRequired
	}
	if a.closed.Load() {
		return Result{}, ErrClosed
	}
	shard := a.table.shard(sessionID)
	shard.mu.Lock()
	entry, found := shard.lookup(sessionID)
	shard.mu.Unlock()
	if !found {
		if a.opts.Store == nil {
			return Result{}, ErrNoCheckpoint
		}
		var err error
		entry, err = a.ensure(ctx, sessionID, "")
		if err != nil {
			return Result{}, err
		}
	}
	shard.mu.Lock()
	running := entry.running
	shard.mu.Unlock()
	if running {
		return Result{}, ErrSessionBusy
	}
	reader, ok := entry.runner.(ResultReader)
	if !ok {
		return Result{}, ErrResultUnsupported
	}
	result, err := reader.Result(ctx)
	if err != nil {
		return Result{}, err
	}
	result.TaskID = entry.taskID
	if result.State == "" {
		result.State = entry.loadState()
	}
	return result, nil
}
