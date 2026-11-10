package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-a2a/wire"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// Config is one remote agent as an engine.
type Config struct {
	// Peer is the remote agent's endpoint.
	Peer string
	// Auth is the credential for reaching it, as written (JSON).
	Auth json.RawMessage
	// Store persists the remote task identity, so a restarted process can still
	// answer a question the peer asked. Nil keeps it in memory only.
	Store agent.Store
	// Identity is the deployment digest a stored task must carry to be resumed.
	Identity runtime.Digest
	// SessionID is the local session this runner serves.
	SessionID string
}

// Runner is a remote A2A agent driven as an engine.
//
// It implements agent.Runner, so a deployment treats a peer exactly like a local
// engine: one session, one task at a time, and a cancel that is forwarded.
type Runner struct {
	client *a2aclient.Client
	store  agent.Store
	digest runtime.Digest
	// session is the local address of the session; the peer's context is
	// separate and lives in remoteContext.
	session string

	mu sync.Mutex
	// taskID is the peer's task id, not the local protocol's.
	taskID a2a.TaskID
	// contextID is the context the peer assigned; a follow-up must carry it.
	remoteContext string
	state         agent.TaskState
}

var _ agent.Runner = (*Runner)(nil)

// New connects to the peer and restores whatever the store remembers about this
// session.
func New(ctx context.Context, cfg Config) (*Runner, error) {
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, errdefs.Validationf("a2a: a session id is required")
	}
	client, err := newClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	runner := &Runner{
		client:  client,
		store:   cfg.Store,
		digest:  cfg.Identity,
		session: cfg.SessionID,
		state:   agent.StateSubmitted,
	}
	runner.restore(ctx)
	return runner, nil
}

// restore reloads the remote task identity and the state the peer last reported.
func (r *Runner) restore(ctx context.Context) {
	if r.store == nil {
		return
	}
	checkpoint, err := r.store.LatestForSession(ctx, r.session)
	if err != nil || checkpoint.Task.ID == "" {
		return
	}
	if r.digest != "" && checkpoint.Runtime != r.digest {
		// A session produced under another deployment is not this one's to
		// continue: the peer or the engine may have changed.
		return
	}
	// The peer's context outlives the task: the next message continues the
	// session even when the task it belonged to finished.
	if raw, ok := checkpoint.Sections["a2a"]; ok {
		var section struct {
			ContextID string `json:"context_id"`
		}
		if json.Unmarshal(raw, &section) == nil {
			r.remoteContext = section.ContextID
		}
	}
	if checkpoint.Task.State.Terminal() {
		// A finished task is not resumed; the context is what carries over.
		return
	}
	r.taskID = a2a.TaskID(checkpoint.Task.ID)
	r.state = checkpoint.Task.State
}

// persist records the remote task identity so a restart can continue it.
func (r *Runner) persist(ctx context.Context) {
	r.mu.Lock()
	taskID := r.taskID
	state := r.state
	contextID := r.remoteContext
	r.mu.Unlock()
	if r.store == nil || taskID == "" {
		return
	}
	section, err := json.Marshal(struct {
		ContextID string `json:"context_id,omitempty"`
	}{contextID})
	if err != nil {
		telemetry.WarnErr(ctx, "a2a: encode task section", err)
		return
	}
	checkpoint := agent.Checkpoint{
		Schema:  agent.CheckpointSchema,
		Runtime: r.digest,
		Task: agent.Task{
			ID:        string(taskID),
			SessionID: r.session,
			State:     state,
		},
		Sections: map[string]json.RawMessage{"a2a": section},
	}
	// Best effort: a proxy whose identity could not be saved still answered the
	// client, and failing the answer over it would be worse than losing a resume
	// point.
	if _, _, err := r.store.Save(ctx, checkpoint, nil); err != nil {
		telemetry.WarnErr(ctx, "a2a: persist the remote task", err)
	}
}

// State reports the lifecycle the peer last reported.
func (r *Runner) State() agent.TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Runner) setState(state agent.TaskState) {
	r.mu.Lock()
	r.state = state
	r.mu.Unlock()
}

// Run forwards one message and streams the peer's events back as core events.
//
// The peer decides whether the message starts a task or answers one: a runner
// state of input-required is carried in the task id, not in a separate call.
func (r *Runner) Run(ctx context.Context, msg message.Message) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		request, err := wire.Message(msg)
		if err != nil {
			yield(nil, err)
			return
		}
		r.mu.Lock()
		request.TaskID = r.taskID
		request.ContextID = r.remoteContext
		r.state = agent.StateWorking
		r.mu.Unlock()

		// The identity must outlive this call however the stream ends.
		defer r.persist(ctx)
		if !yield(agent.StreamStart{Model: "a2a"}, nil) {
			return
		}
		for event, streamErr := range r.client.SendStreamingMessage(ctx,
			&a2a.SendMessageRequest{Message: request}) {
			if streamErr != nil {
				if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
					r.setState(agent.StateCanceled)
					yield(agent.Status{State: agent.StateCanceled, Reason: streamErr.Error()}, nil)
					return
				}
				r.setState(agent.StateFailed)
				yield(agent.Status{State: agent.StateFailed, Reason: streamErr.Error()}, nil)
				// The cause must reach the caller: a status alone would make a
				// protocol failure indistinguishable from a refused task.
				yield(nil, streamErr)
				return
			}
			done, handleErr := r.handle(event, yield)
			if handleErr != nil {
				yield(nil, handleErr)
				return
			}
			if done {
				return
			}
		}
		// A stream that ends while still working completed without saying so; a
		// paused stream ended exactly where it meant to, and overwriting that
		// with "completed" would lose the question the peer asked.
		switch state := r.State(); state {
		case agent.StateSubmitted, agent.StateWorking:
			r.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
}

// handle maps one wire event onto core events, and reports whether the task
// reached a terminal state.
func (r *Runner) handle(event a2a.Event, yield func(agent.Event, error) bool) (bool, error) {
	switch typed := event.(type) {
	case *a2a.Task:
		r.mu.Lock()
		if typed.ID != "" {
			r.taskID = typed.ID
		}
		if typed.ContextID != "" {
			r.remoteContext = typed.ContextID
		}
		state := wire.FromState(typed.Status.State)
		r.mu.Unlock()
		r.setState(state)
		if !yield(agent.Status{State: state}, nil) {
			return true, nil
		}
		return state.Terminal(), nil
	case *a2a.TaskStatusUpdateEvent:
		r.mu.Lock()
		if typed.ContextID != "" {
			r.remoteContext = typed.ContextID
		}
		r.mu.Unlock()
		state := wire.FromState(typed.Status.State)
		r.setState(state)
		if text := wire.Text(typed.Status.Message); text != "" {
			if !yield(agent.Text{Text: text}, nil) {
				return true, nil
			}
		}
		if !yield(agent.Status{State: state}, nil) {
			return true, nil
		}
		return state.Terminal(), nil
	case *a2a.TaskArtifactUpdateEvent:
		if typed.Artifact == nil {
			return false, nil
		}
		return false, r.handleArtifact(typed.Artifact, yield)
	case *a2a.Message:
		if text := wire.Text(typed); text != "" {
			if !yield(agent.Text{Text: text}, nil) {
				return true, nil
			}
		}
		return false, nil
	default:
		// Tool calls and usage have no local equivalent on a proxied task; the
		// peer owns that work.
		return false, nil
	}
}

// handleArtifact turns one artifact chunk into answer text or a produced
// artifact. Text arrives chunked (the peer's server appends one part per delta),
// so each text part is a delta of the answer.
func (r *Runner) handleArtifact(remote *a2a.Artifact, yield func(agent.Event, error) bool) error {
	produced := false
	for _, part := range remote.Parts {
		if text, ok := part.Content.(a2a.Text); ok {
			if string(text) == "" {
				continue
			}
			if !yield(agent.Text{Text: string(text)}, nil) {
				return nil
			}
			continue
		}
		produced = true
	}
	if produced && !yield(agent.ArtifactWritten{Artifact: artifactOf(remote)}, nil) {
		return nil
	}
	return nil
}

// Cancel asks the peer to stop the task.
func (r *Runner) Cancel(ctx context.Context, reason string) error {
	r.mu.Lock()
	taskID := r.taskID
	state := r.state
	r.mu.Unlock()
	if taskID == "" || state.Terminal() {
		// Nothing to stop: no task yet, or it already finished.
		return nil
	}
	if _, err := r.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID}); err != nil {
		return err
	}
	r.setState(agent.StateCanceled)
	r.persist(ctx)
	return nil
}

// Close releases the runner. The client holds no connection pool of its own.
func (r *Runner) Close() error { return nil }
