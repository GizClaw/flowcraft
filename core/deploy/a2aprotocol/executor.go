package a2aprotocol

import (
	"context"
	"iter"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"
	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// executorConfig is what an executed deployment supplies: the card it
// answers with, the session table it drives, and the artifact URL rule.
type executorConfig struct {
	Card  cardConfig
	Agent *agent.Agent
	// ArtifactURL turns a blob reference into a URL a client can fetch. Nil
	// keeps the reference in a data part: a client then learns the pointer
	// instead of a file URL that nobody can open.
	ArtifactURL func(ref string) string
}

// runningRun is one execution in flight: the cancel that stops it.
type runningRun struct {
	cancel context.CancelFunc
}

// executor implements a2asrv.AgentExecutor over an agent session table.
//
// The two shapes meet unusually well: both are iterators of events, so this is a
// translation and not a state machine. The table owns task identity and the run
// slot; the executor only forwards a client cancel and remembers one that
// arrived before the execution started.
type executor struct {
	card        *a2a.AgentCard
	agent       *agent.Agent
	artifactURL func(ref string) string

	mu sync.Mutex
	// running maps a task id to its execution. A cancel that finds nothing here
	// means the run already ended.
	running map[string]*runningRun
	// cancelled remembers ids cancelled before their execution registered.
	cancelled map[string]struct{}
}

var _ a2asrv.AgentExecutor = (*executor)(nil)

// newExecutor validates the card and the session table.
func newExecutor(cfg executorConfig) (*executor, error) {
	if cfg.Agent == nil {
		return nil, errdefs.Validationf("a2a: an agent is required")
	}
	card, err := newCard(cfg.Card)
	if err != nil {
		return nil, err
	}
	return &executor{
		card:        card,
		agent:       cfg.Agent,
		artifactURL: cfg.ArtifactURL,
		running:     make(map[string]*runningRun),
		cancelled:   make(map[string]struct{}),
	}, nil
}

// Card is the AgentCard this server answers with.
func (e *executor) Card() *a2a.AgentCard { return e.card }

// Execute runs one Task and streams its events.
func (e *executor) Execute(ctx context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if exec == nil || exec.Message == nil {
			yield(nil, errdefs.Validationf("a2a: execution has no message"))
			return
		}
		turn, err := wire.FromMessage(exec.Message)
		if err != nil {
			yield(nil, err)
			return
		}
		taskID := string(exec.TaskID)
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !e.register(taskID, cancel) {
			// A cancel arrived before the execution started.
			cancel()
		}
		defer e.unregister(taskID)

		info := exec.TaskInfo()
		// The server requires a new stream to open with a Task; a stored one is
		// already tracked, and re-announcing it would reset its state and history
		// to "submitted".
		if exec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(info, exec.Message), nil) {
				return
			}
		}
		// A message to a task that is waiting for input is the answer to its
		// question: the runner's state decides that, not this adapter.
		stream := newTextStream(info, e.artifactURL)
		for event, runErr := range e.agent.Run(runCtx, exec.ContextID, turn, agent.WithTaskID(taskID)) {
			if runErr != nil {
				for _, converted := range stream.close() {
					if !yield(converted, nil) {
						return
					}
				}
				// The loop reports its own failures as a status event; an error
				// here means it could not even report, so say so to the client.
				yield(a2a.NewStatusUpdateEvent(info, a2a.TaskStateFailed,
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(runErr.Error()))), nil)
				return
			}
			for _, converted := range stream.events(event) {
				if !yield(converted, nil) {
					return
				}
			}
		}
		for _, converted := range stream.close() {
			if !yield(converted, nil) {
				return
			}
		}
	}
}

// Cancel signals the running execution of one task.
//
// The table's Cancel is the only path: it stops a live run, and for a paused or
// stored task it records the terminal state where the engine's checkpoint
// lives. A cancellation that reaches a live run emits nothing itself: the run
// reports the canceled state as its own terminal event, and a second terminal
// event would be rejected by the server. Only a task with nothing in flight
// answers directly.
func (e *executor) Cancel(ctx context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if exec == nil {
			return
		}
		live := e.signal(string(exec.TaskID))
		// A failure here is the client's answer: reporting canceled when the
		// engine refused would leave the two records disagreeing.
		if err := e.agent.Cancel(ctx, exec.ContextID, "client cancelled"); err != nil {
			yield(nil, err)
			return
		}
		if live {
			return
		}
		yield(a2a.NewStatusUpdateEvent(exec.TaskInfo(), a2a.TaskStateCanceled, nil), nil)
	}
}

// register records a running execution; it reports false when the task was
// already cancelled.
func (e *executor) register(taskID string, cancel context.CancelFunc) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, cancelled := e.cancelled[taskID]; cancelled {
		delete(e.cancelled, taskID)
		return false
	}
	e.running[taskID] = &runningRun{cancel: cancel}
	return true
}

func (e *executor) unregister(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.running, taskID)
}

// signal stops a running execution. It reports whether a run was found; when
// none was, the id is remembered so a cancel racing the registration wins.
func (e *executor) signal(taskID string) bool {
	e.mu.Lock()
	run, running := e.running[taskID]
	if !running {
		e.cancelled[taskID] = struct{}{}
	}
	e.mu.Unlock()
	if !running {
		return false
	}
	run.cancel()
	return true
}
