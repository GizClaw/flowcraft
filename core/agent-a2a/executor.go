package a2a

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Runner is what an execution drives. *agent.Session is the local
// implementation; a proxy over a remote agent implements the same three methods,
// which is what lets a deployment expose either one through the same protocol.
type Runner interface {
	// State is the runner's lifecycle state; the executor only needs this to
	// decide whether an incoming message answers a pending question.
	State() agent.TaskState
	// SetIdentity stamps the protocol's task id and context onto the runner
	// before the Run starts.
	SetIdentity(taskID, conversationID string)
	Run(ctx context.Context, in agent.Input) iter.Seq2[agent.Event, error]
	Resume(ctx context.Context, reply agent.Reply) iter.Seq2[agent.Event, error]
}

// Sessions builds the runner one execution drives.
//
// A fresh runner per execution keeps conversations apart without the executor
// tracking anything: the SDK owns task identity, and the host owns whatever it
// needs to build one — a runtime, a store, an interrupt channel.
type Sessions func(ctx context.Context, exec *a2asrv.ExecutorContext) (Runner, error)

// Config is one A2A server's two halves: what it claims, and how it runs.
type Config struct {
	Card     CardConfig
	Sessions Sessions
	// OnUndelivered receives input the Task did not consume. The loop hands it
	// up rather than dropping it; the deployment decides whether the
	// conversation continues.
	OnUndelivered func(conversationID string, messages []message.Message)
	// OnCancel is called when a client cancels a task, after the local run has
	// been signalled. A deployment uses it for work the local context cannot
	// reach — a remote peer's task, for instance.
	OnCancel func(conversationID string)
}

// Executor implements a2asrv.AgentExecutor over agent.Session.Run.
//
// The two shapes meet unusually well: both are iterators of events, so this is a
// translation and not a state machine.
type Executor struct {
	card  *a2a.AgentCard
	build Sessions
	cfg   Config

	mu sync.Mutex
	// running maps a task id to the cancel func of its execution. A cancel that
	// finds nothing here means the run already ended.
	running map[string]context.CancelFunc
	// cancelled remembers ids cancelled before their execution registered.
	cancelled map[string]struct{}
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// New validates the card and the session builder.
func New(cfg Config) (*Executor, error) {
	if cfg.Sessions == nil {
		return nil, errdefs.Validationf("a2a: a session builder is required")
	}
	card, err := NewCard(cfg.Card)
	if err != nil {
		return nil, err
	}
	return &Executor{
		card:      card,
		build:     cfg.Sessions,
		cfg:       cfg,
		running:   make(map[string]context.CancelFunc),
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
		turn, err := fromA2AMessage(exec.Message)
		if err != nil {
			yield(nil, err)
			return
		}
		session, err := e.build(ctx, exec)
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
		// The SDK owns task identity; adopting it means a checkpoint resumed
		// through A2A keeps the id its client knows.
		session.SetIdentity(string(exec.TaskID), exec.ContextID)

		info := exec.TaskInfo()
		// The server requires a new stream to open with a Task; a stored one is
		// already tracked, and re-announcing it would reset its state and history
		// to "submitted".
		if exec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(info, exec.Message), nil) {
				return
			}
		}
		// A message addressed to a task that is waiting for input is the answer
		// to its question, not a new task: resuming keeps the pending call. The
		// durable state and the runner have to agree, or a stale row would fail
		// a recoverable task.
		var sequence iter.Seq2[agent.Event, error]
		stored := exec.StoredTask
		if stored != nil && stored.Status.State == a2a.TaskStateInputRequired {
			if session.State() != agent.StateInputRequired {
				yield(nil, errdefs.Validationf(
					"a2a: task %s is input-required in the store but %s in the runner",
					stored.ID, session.State()))
				return
			}
			sequence = session.Resume(runCtx, agent.Reply{Text: textContent(exec.Message)})
		} else {
			sequence = session.Run(runCtx, agent.Input{Messages: []message.Message{turn}})
		}
		for event, runErr := range sequence {
			if runErr != nil {
				// The loop reports its own failures as a status event; an error
				// here means it could not even report, so say so to the client.
				yield(a2a.NewStatusUpdateEvent(info, a2a.TaskStateFailed,
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(runErr.Error()))), nil)
				return
			}
			if undelivered, ok := event.(agent.Undelivered); ok {
				if e.cfg.OnUndelivered != nil && len(undelivered.Messages) > 0 {
					e.cfg.OnUndelivered(exec.ContextID, undelivered.Messages)
				}
				continue
			}
			converted, representable := toA2A(info, event)
			if !representable {
				continue
			}
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
func (e *Executor) Cancel(_ context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if exec == nil {
			return
		}
		taskID := string(exec.TaskID)
		if e.signal(taskID) {
			if e.cfg.OnCancel != nil {
				e.cfg.OnCancel(exec.ContextID)
			}
			return
		}
		if e.cfg.OnCancel != nil {
			e.cfg.OnCancel(exec.ContextID)
		}
		yield(a2a.NewStatusUpdateEvent(exec.TaskInfo(), a2a.TaskStateCanceled, nil), nil)
	}
}

// register records a running execution; it reports false when the task was
// already cancelled.
func (e *Executor) register(taskID string, cancel context.CancelFunc) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, cancelled := e.cancelled[taskID]; cancelled {
		delete(e.cancelled, taskID)
		return false
	}
	e.running[taskID] = cancel
	return true
}

func (e *Executor) unregister(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.running, taskID)
}

// signal cancels a running execution, and reports whether one was found.
func (e *Executor) signal(taskID string) bool {
	e.mu.Lock()
	cancel, running := e.running[taskID]
	if !running {
		e.cancelled[taskID] = struct{}{}
	}
	e.mu.Unlock()
	if running {
		cancel()
	}
	return running
}

// toA2A maps one loop event onto the wire.
//
// Tool calls, tool results, usage and reasoning do not cross: they are the
// agent's working, and A2A has no vocabulary for them. Answer text crosses as an
// appending status update carrying a message, which is how partial output
// reaches a client.
func toA2A(info a2a.TaskInfoProvider, event agent.Event) (a2a.Event, bool) {
	switch typed := event.(type) {
	case agent.Text:
		if typed.Text == "" {
			return nil, false
		}
		return a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(typed.Text))), true
	case agent.Status:
		return a2a.NewStatusUpdateEvent(info, stateOf(typed.State), reasonMessage(typed.Reason)), true
	case agent.ArtifactWritten:
		return a2a.NewArtifactEvent(info,
			a2a.NewFileURLPart(a2a.URL(typed.Artifact.Blob), typed.Artifact.MediaType)), true
	default:
		return nil, false
	}
}

func reasonMessage(reason string) *a2a.Message {
	if reason == "" {
		return nil
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(reason))
}

// stateOf translates the internal lifecycle into the protocol's spelling. The
// internal names are the protocol's words in lower case; the SDK uses the
// proto-style upper-case constants.
func stateOf(state agent.TaskState) a2a.TaskState {
	switch state {
	case agent.StateSubmitted:
		return a2a.TaskStateSubmitted
	case agent.StateWorking:
		return a2a.TaskStateWorking
	case agent.StateInputRequired:
		return a2a.TaskStateInputRequired
	case agent.StateCompleted:
		return a2a.TaskStateCompleted
	case agent.StateCanceled:
		return a2a.TaskStateCanceled
	case agent.StateRejected:
		return a2a.TaskStateRejected
	case agent.StateAuthRequired:
		return a2a.TaskStateAuthRequired
	default:
		return a2a.TaskStateFailed
	}
}

// textContent is the text parts of a wire message, concatenated.
func textContent(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	var out strings.Builder
	for _, part := range msg.Parts {
		if text, ok := part.Content.(a2a.Text); ok {
			out.WriteString(string(text))
		}
	}
	return out.String()
}

// fromA2AMessage converts an incoming wire message into the core shape.
func fromA2AMessage(msg *a2a.Message) (message.Message, error) {
	role := message.RoleAssistant
	if msg.Role == a2a.MessageRoleUser {
		role = message.RoleUser
	}
	parts := make([]message.Part, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		converted, err := fromA2APart(part)
		if err != nil {
			return message.Message{}, err
		}
		parts = append(parts, converted)
	}
	return message.Message{Role: role, Content: message.Content{Parts: parts}}, nil
}

// fromA2APart converts one wire part. A content type this adapter cannot carry
// is an error rather than a dropped part: a client cannot tell an ignored
// attachment from one that was never sent.
func fromA2APart(part *a2a.Part) (message.Part, error) {
	if part == nil {
		return nil, errdefs.Validationf("a2a: nil part")
	}
	switch content := part.Content.(type) {
	case a2a.Text:
		return message.TextPart{Text: string(content)}, nil
	case a2a.URL:
		return message.FilePart{
			URI:       string(content),
			MediaType: part.MediaType,
			Name:      part.Filename,
		}, nil
	case a2a.Raw:
		// Inline bytes have no core part: a core message either carries a URL or
		// a parsed data value. Saying so beats attaching bytes to nothing.
		return nil, errdefs.Validationf(
			"a2a: inline bytes are not carried; upload the file and send its URL")
	case a2a.Data:
		body, err := json.Marshal(content)
		if err != nil {
			return nil, errdefs.Validationf("a2a: encode data part: %w", err)
		}
		return message.DataPart{MediaType: part.MediaType, Value: body}, nil
	default:
		return nil, errdefs.Validationf(
			"a2a: part content %T has no core representation", part.Content)
	}
}
