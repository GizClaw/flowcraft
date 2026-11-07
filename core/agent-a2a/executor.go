package a2a

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Sessions builds the agent session one execution runs on.
//
// A fresh session per execution keeps conversations apart without the executor
// tracking anything: the SDK owns task identity, and the host owns whatever it
// needs to build a session for it — a runtime, a store, an interrupt channel.
type Sessions func(ctx context.Context, exec *a2asrv.ExecutorContext) (*agent.Session, error)

// Config is one A2A server's two halves: what it claims, and how it runs.
type Config struct {
	Card     CardConfig
	Sessions Sessions
}

// Executor implements a2asrv.AgentExecutor over agent.Session.Run.
//
// The two shapes meet unusually well: both are iterators of events, so this is a
// translation and not a state machine.
type Executor struct {
	card  *a2a.AgentCard
	build Sessions
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
	return &Executor{card: card, build: cfg.Sessions}, nil
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
		// The SDK owns task identity; adopting it means a checkpoint resumed
		// through A2A keeps the id its client knows.
		session.Task().ID = string(exec.TaskID)
		session.Task().ContextID = exec.ContextID

		info := exec.TaskInfo()
		for event, runErr := range session.Run(ctx, agent.Input{Messages: []message.Message{turn}}) {
			if runErr != nil {
				// The loop reports its own failures as a status event; an error
				// here means it could not even report, so say so to the client.
				yield(a2a.NewStatusUpdateEvent(info, a2a.TaskStateFailed,
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(runErr.Error()))), nil)
				return
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

// Cancel answers a client's stop request. The loop notices through its interrupt
// channel when the host wired one; this method reports the state the client
// asked for.
func (e *Executor) Cancel(_ context.Context, exec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(exec.TaskInfo(), a2a.TaskStateCanceled, nil), nil)
	}
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
