package server

import (
	"context"
	"encoding/gob"
	"iter"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/internal/wire"
	"github.com/GizClaw/flowcraft/core/errdefs"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Sessions builds the runner one execution drives.
//
// The runner is the engine contract, not a protocol-specific one: an execution
// hands it a message and forwards the events it produces. The host owns the
// runner's lifetime and its Close; the executor never closes it.
type Runners func(ctx context.Context, exec *a2asrv.ExecutorContext) (agent.Runner, error)

// Config is one A2A server's two halves: what it claims, and how it runs.
type Config struct {
	Card    CardConfig
	Runners Runners
	// ArtifactURL turns a blob reference into a URL a client can fetch. Nil
	// keeps the reference in a data part: a client then learns the pointer
	// instead of a file URL that nobody can open.
	ArtifactURL func(ref string) string
}

// runningRun is one execution in flight: the cancel that stops it and the
// runner a client cancel has to reach.
type runningRun struct {
	cancel context.CancelFunc
	runner agent.Runner
}

// Executor implements a2asrv.AgentExecutor over agent.Runner.
//
// The two shapes meet unusually well: both are iterators of events, so this is a
// translation and not a state machine.
type Executor struct {
	card  *a2a.AgentCard
	build Runners
	cfg   Config

	mu sync.Mutex
	// running maps a task id to its execution. A cancel that finds nothing here
	// means the run already ended.
	running map[string]*runningRun
	// cancelled remembers ids cancelled before their execution registered.
	cancelled map[string]struct{}
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

func init() {
	// The SDK's in-memory task store gob-encodes tasks, and a data part's value
	// is an interface: gob needs the concrete type registered before it can
	// carry the reference a produced artifact falls back to.
	gob.Register(map[string]string{})
}

// New validates the card and the session builder.
func New(cfg Config) (*Executor, error) {
	if cfg.Runners == nil {
		return nil, errdefs.Validationf("a2a: a session builder is required")
	}
	card, err := NewCard(cfg.Card)
	if err != nil {
		return nil, err
	}
	return &Executor{
		card:      card,
		build:     cfg.Runners,
		cfg:       cfg,
		running:   make(map[string]*runningRun),
		cancelled: make(map[string]struct{}),
	}, nil
}

// Card is the AgentCard this server answers with.
func (e *Executor) Card() *a2a.AgentCard { return e.card }

// Execute runs one Task and streams its events.
func (e *Executor) Execute(ctx context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
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
		runner, err := e.build(ctx, exec)
		if err != nil {
			yield(nil, err)
			return
		}
		taskID := string(exec.TaskID)
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if !e.register(taskID, cancel, runner) {
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
		stream := newTextStream(info, e.cfg.ArtifactURL)
		for event, runErr := range runner.Run(runCtx, turn) {
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
// A cancellation that reaches a live run emits nothing itself: the run reports
// the canceled state as its own terminal event, and a second terminal event
// would be rejected by the server. Only a task with nothing to cancel answers
// directly.
func (e *Executor) Cancel(ctx context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if exec == nil {
			return
		}
		if runner, live := e.signal(string(exec.TaskID)); live {
			// The context stops the stream; Cancel lets the engine record the
			// terminal state where its checkpoint lives.
			if err := runner.Cancel(ctx, "client cancelled"); err != nil {
				yield(nil, err)
			}
			return
		}
		// No run in flight: the task may be paused, and the engine still has to
		// record the cancellation or a restart would resume it as if nothing
		// happened. A failure here is the client's answer: reporting canceled
		// when the engine refused would leave the two records disagreeing.
		built, err := e.build(ctx, exec)
		if err != nil {
			yield(nil, err)
			return
		}
		if built != nil {
			if err := built.Cancel(ctx, "client cancelled"); err != nil {
				yield(nil, err)
				return
			}
		}
		yield(a2a.NewStatusUpdateEvent(exec.TaskInfo(), a2a.TaskStateCanceled, nil), nil)
	}
}

// register records a running execution; it reports false when the task was
// already cancelled.
func (e *Executor) register(taskID string, cancel context.CancelFunc, runner agent.Runner) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, cancelled := e.cancelled[taskID]; cancelled {
		delete(e.cancelled, taskID)
		return false
	}
	e.running[taskID] = &runningRun{cancel: cancel, runner: runner}
	return true
}

func (e *Executor) unregister(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.running, taskID)
}

// signal stops a running execution and returns its runner, so a client cancel
// can reach the engine. It reports whether a run was found.
func (e *Executor) signal(taskID string) (agent.Runner, bool) {
	e.mu.Lock()
	run, running := e.running[taskID]
	if !running {
		e.cancelled[taskID] = struct{}{}
	}
	e.mu.Unlock()
	if !running {
		return nil, false
	}
	run.cancel()
	return run.runner, true
}

// textStream turns a run's text into one artifact per model round.
//
// Text is not a status message: a status carries the latest status, so emitting
// one message per delta would shred the answer across the task history. An
// artifact chunked with Append/LastChunk keeps one artifact per round, which is
// what tasks/get later serves as the complete answer.
//
// One chunk is held back so the round's last chunk can carry LastChunk itself:
// the protocol rejects an artifact update without parts, so closing cannot be a
// separate empty event.
type textStream struct {
	info        a2a.TaskInfoProvider
	artifactURL func(ref string) string
	id          a2a.ArtifactID
	open        bool
	pending     string
}

func newTextStream(info a2a.TaskInfoProvider, artifactURL func(string) string) *textStream {
	return &textStream{info: info, artifactURL: artifactURL}
}

// events maps one run event onto zero or more wire events.
func (s *textStream) events(event agent.Event) []a2a.Event {
	switch typed := event.(type) {
	case agent.StreamStart:
		// A new round starts a new artifact.
		return s.close()
	case agent.Text:
		if typed.Text == "" {
			return nil
		}
		if s.pending == "" {
			s.pending = typed.Text
			return nil
		}
		out := s.chunk(s.pending, false)
		s.pending = typed.Text
		return out
	case agent.ArtifactWritten:
		return []a2a.Event{s.artifact(typed.Artifact)}
	case agent.Status:
		var out []a2a.Event
		if typed.State.Terminal() {
			out = s.close()
		}
		return append(out, a2a.NewStatusUpdateEvent(s.info, wire.State(typed.State), reasonMessage(typed.Reason)))
	default:
		// Tool calls, tool results, usage and reasoning are the agent's working;
		// the protocol has no vocabulary for them.
		return nil
	}
}

// chunk renders one piece of the answer. The first piece of a round opens the
// artifact; later ones append to it.
func (s *textStream) chunk(text string, last bool) []a2a.Event {
	appendChunk := s.open
	if !s.open {
		s.id = a2a.NewArtifactID()
		s.open = true
	}
	return []a2a.Event{&a2a.TaskArtifactUpdateEvent{
		TaskID:    s.info.TaskInfo().TaskID,
		ContextID: s.info.TaskInfo().ContextID,
		Append:    appendChunk,
		LastChunk: last,
		Artifact: &a2a.Artifact{
			ID:    s.id,
			Parts: a2a.ContentParts{a2a.NewTextPart(text)},
		},
	}}
}

// close ends the open artifact, if any, so a client knows the answer is
// complete. The next round starts a fresh artifact.
func (s *textStream) close() []a2a.Event {
	if s.pending == "" {
		return nil
	}
	text := s.pending
	s.pending = ""
	events := s.chunk(text, true)
	s.open = false
	return events
}

// artifact renders one produced artifact: a fetchable URL when the host can
// serve the bytes, the reference itself otherwise.
func (s *textStream) artifact(produced agent.Artifact) a2a.Event {
	artifact := &a2a.Artifact{
		ID:   a2a.ArtifactID(produced.ID),
		Name: produced.Name,
	}
	if artifact.ID == "" {
		artifact.ID = a2a.NewArtifactID()
	}
	if s.artifactURL != nil {
		if url := s.artifactURL(produced.Blob); url != "" {
			artifact.Parts = a2a.ContentParts{
				a2a.NewFileURLPart(a2a.URL(url), produced.MediaType),
			}
		}
	}
	if artifact.Parts == nil {
		artifact.Parts = a2a.ContentParts{a2a.NewDataPart(map[string]string{
			"blob":       produced.Blob,
			"media_type": produced.MediaType,
			"name":       produced.Name,
		})}
	}
	return &a2a.TaskArtifactUpdateEvent{
		TaskID:    s.info.TaskInfo().TaskID,
		ContextID: s.info.TaskInfo().ContextID,
		LastChunk: true,
		Artifact:  artifact,
	}
}

func reasonMessage(reason string) *a2a.Message {
	if reason == "" {
		return nil
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(reason))
}

// metadataOf carries the transport's annotations into the core message: the
// cited tasks, the extensions, the free-form metadata, and each part's own

// fromRawPart carries inline bytes into the core part that matches them: media

// fromA2APart converts one wire part. A content type this adapter cannot carry
// is an error rather than a dropped part: a client cannot tell an ignored
