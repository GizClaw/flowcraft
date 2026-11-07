package deploy

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// proxyConversation is an agent that owns no loop: every request is forwarded to
// a remote A2A agent, and the remote task is the local task.
//
// It implements the same surface manage drives, so the manager does not branch
// per engine kind beyond construction.
type proxyConversation struct {
	client       *a2aclient.Client
	conversation string
	// store and digest persist the remote task identity, so a restarted process
	// can still answer a question the remote agent asked.
	store  agent.Store
	digest runtime.Digest

	mu     sync.Mutex
	taskID a2a.TaskID
	// remoteContext is the context the remote server assigned. A follow-up
	// message must carry it: the server rejects a message whose context does not
	// match the task's.
	remoteContext string
	state         agent.TaskState
	// queue holds messages not yet forwarded. A proxy cannot steer a remote
	// task mid-flight, so steering degrades to queueing and says so.
	queue []message.Message
}

// newProxyConversation connects to the peer declared by the a2a engine and
// restores whatever the store remembers about this conversation.
func newProxyConversation(
	ctx context.Context,
	spec EngineSpec,
	store agent.Store,
	digest runtime.Digest,
	conversationID string,
) (*proxyConversation, error) {
	if strings.TrimSpace(spec.Peer) == "" {
		return nil, errdefs.Validationf("manage: the a2a engine needs a peer")
	}
	options := make([]a2aclient.FactoryOption, 0, 1)
	if token := bearerToken(spec.Auth); token != "" {
		options = append(options, a2aclient.WithJSONRPCTransport(&http.Client{
			Transport: bearerTransport{token: token, base: http.DefaultTransport},
		}))
	}
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{
		a2a.NewAgentInterface(spec.Peer, a2a.TransportProtocolJSONRPC),
	}, options...)
	if err != nil {
		return nil, errdefs.Validationf("manage: connect to %s: %w", spec.Peer, err)
	}
	conversation := &proxyConversation{
		client:       client,
		conversation: conversationID,
		store:        store,
		digest:       digest,
		state:        agent.StateSubmitted,
	}
	conversation.restore(ctx)
	return conversation, nil
}

// restore reloads the remote task identity from the store.
func (p *proxyConversation) restore(ctx context.Context) {
	if p.store == nil {
		return
	}
	checkpoint, err := p.store.LatestForContext(ctx, p.conversation)
	if err != nil || checkpoint.Task.ID == "" {
		return
	}
	if checkpoint.Runtime != p.digest {
		// A conversation produced under another deployment is not this one's to
		// continue: the peer or the engine may have changed.
		return
	}
	p.taskID = a2a.TaskID(checkpoint.Task.ID)
	p.state = checkpoint.Task.State
	if raw, ok := checkpoint.Sections["a2a"]; ok {
		var section struct {
			ContextID string `json:"context_id"`
		}
		if json.Unmarshal(raw, &section) == nil {
			p.remoteContext = section.ContextID
		}
	}
}

// persist records the remote task identity so a restart can continue it.
func (p *proxyConversation) persist(ctx context.Context) {
	if p.store == nil {
		return
	}
	p.mu.Lock()
	taskID := p.taskID
	state := p.state
	remoteContext := p.remoteContext
	p.mu.Unlock()
	if taskID == "" {
		return
	}
	section, err := json.Marshal(map[string]string{"context_id": remoteContext})
	if err != nil {
		return
	}
	checkpoint := agent.Checkpoint{
		Schema:  agent.CheckpointSchema,
		Runtime: p.digest,
		Task: agent.Task{
			ID:        string(taskID),
			ContextID: p.conversation,
			State:     state,
		},
		Sections: map[string]json.RawMessage{"a2a": section},
	}
	// Best effort: a proxy whose state could not be saved still answered the
	// client, and failing the answer over it would be worse than losing a resume
	// point.
	_ = p.store.Save(ctx, checkpoint, nil)
}

// bearerTransport attaches the bearer token to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// bearerToken reads the deployment's credential. Only bearer is understood: an
// auth block this build cannot honour must not be silently ignored.
func bearerToken(auth map[string]any) string {
	if len(auth) == 0 {
		return ""
	}
	if kind, _ := auth["type"].(string); kind != "" && kind != "bearer" {
		return ""
	}
	token, _ := auth["token"].(string)
	return token
}

// SetIdentity keeps the remote task id and takes only the conversation.
//
// The id the local protocol assigned is not the peer's task id: a follow-up must
// address the task the peer issued, so adopting the local one would send the
// peer an id it never created.
func (p *proxyConversation) SetIdentity(_ string, conversationID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if conversationID != "" {
		p.conversation = conversationID
	}
}

// State reports the remote task's state.
func (p *proxyConversation) State() agent.TaskState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Task is the local view of the remote task.
func (p *proxyConversation) Task() *agent.Task {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &agent.Task{
		ID:        string(p.taskID),
		ContextID: p.conversation,
		State:     p.state,
	}
}

// Send queues the message. A proxy has nothing to steer locally; the effective
// mode says so instead of pretending.
func (p *proxyConversation) Send(_ context.Context, msg message.Message, mode agent.Mode) (agent.Mode, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, msg)
	return agent.ModeQueue, nil
}

// Pending reports queued messages.
func (p *proxyConversation) Pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Abandon gives up on a remote task that is waiting for input.
func (p *proxyConversation) Abandon() error {
	p.mu.Lock()
	taskID := p.taskID
	p.state = agent.StateCanceled
	p.mu.Unlock()
	if taskID != "" {
		// Best effort: a cancel that fails leaves the remote task running, which
		// is the remote agent's business, not a local error.
		_, _ = p.client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: taskID})
	}
	p.persist(context.Background())
	return nil
}

// Run forwards one message to the peer and streams its events back.
func (p *proxyConversation) Run(ctx context.Context, in agent.Input) iter.Seq2[agent.Event, error] {
	return p.forward(ctx, in.Messages)
}

// Resume continues the remote task with the user's answer.
func (p *proxyConversation) Resume(ctx context.Context, reply agent.Reply) iter.Seq2[agent.Event, error] {
	return p.forward(ctx, []message.Message{message.NewTextMessage(message.RoleUser, reply.Text)})
}

func (p *proxyConversation) forward(ctx context.Context, messages []message.Message) iter.Seq2[agent.Event, error] {
	p.mu.Lock()
	queued := p.queue
	p.queue = nil
	taskID := p.taskID
	p.state = agent.StateWorking
	p.mu.Unlock()
	messages = append(queued, messages...)
	if len(messages) == 0 {
		return func(yield func(agent.Event, error) bool) {
			yield(nil, errdefs.Validationf("manage: nothing to forward"))
		}
	}

	parts := make([]*a2a.Part, 0, len(messages))
	for _, msg := range messages {
		converted, err := toA2AParts(msg)
		if err != nil {
			return func(yield func(agent.Event, error) bool) { yield(nil, err) }
		}
		parts = append(parts, converted...)
	}
	var msg *a2a.Message
	if taskID != "" {
		msg = a2a.NewMessageForTask(a2a.MessageRoleUser, p, parts...)
	} else {
		msg = a2a.NewMessage(a2a.MessageRoleUser, parts...)
	}

	return func(yield func(agent.Event, error) bool) {
		// Persist however the stream ends, including the terminal status path
		// that returns early: the task identity must outlive this call.
		defer p.persist(ctx)
		if !yield(agent.StreamStart{Model: "a2a"}, nil) {
			return
		}
		for event, err := range p.client.SendStreamingMessage(ctx,
			&a2a.SendMessageRequest{Message: msg}) {
			if err != nil {
				p.setState(agent.StateFailed)
				if !yield(agent.Status{State: agent.StateFailed, Reason: err.Error()}, nil) {
					return
				}
				// The cause must reach the caller: a status alone would make a
				// protocol failure indistinguishable from a refused task.
				yield(nil, err)
				return
			}
			if done := p.handle(event, yield); done {
				return
			}
		}
		// A stream that ends while still working completed without saying so; a
		// paused stream ended exactly where it meant to, and overwriting that with
		// "completed" would lose the question the remote agent asked.
		switch state := p.currentState(); state {
		case agent.StateSubmitted, agent.StateWorking:
			p.setState(agent.StateCompleted)
			yield(agent.Status{State: agent.StateCompleted}, nil)
		}
	}
}

// handle maps one remote event onto local events. It reports whether the stream
// is finished.
func (p *proxyConversation) handle(event a2a.Event, yield func(agent.Event, error) bool) bool {
	switch typed := event.(type) {
	case *a2a.Task:
		p.mu.Lock()
		p.taskID = typed.ID
		if typed.ContextID != "" {
			p.remoteContext = typed.ContextID
		}
		p.mu.Unlock()
		return false
	case *a2a.TaskStatusUpdateEvent:
		p.mu.Lock()
		if typed.ContextID != "" {
			p.remoteContext = typed.ContextID
		}
		p.mu.Unlock()
		state := fromA2AState(typed.Status.State)
		p.setState(state)
		if text := textOf(typed.Status.Message); text != "" {
			if !yield(agent.Text{Text: text}, nil) {
				return true
			}
		}
		if !yield(agent.Status{State: state}, nil) {
			return true
		}
		return state.Terminal()
	case *a2a.TaskArtifactUpdateEvent:
		if typed.Artifact == nil {
			return false
		}
		if !yield(agent.ArtifactWritten{Artifact: artifactOf(typed.Artifact)}, nil) {
			return true
		}
		return false
	case *a2a.Message:
		if text := textOf(typed); text != "" {
			if !yield(agent.Text{Text: text}, nil) {
				return true
			}
		}
		return false
	default:
		// Tool calls and usage have no local equivalent on a proxied task; the
		// remote agent owns that work.
		return false
	}
}

// CancelRemote asks the peer to cancel the task this conversation is on.
//
// A local context cancellation stops our stream but not the peer's work, so a
// deployment-level cancel has to reach across. Best effort: a peer that refuses
// the cancel is the peer's business, and the local run is already stopping.
func (p *proxyConversation) CancelRemote(ctx context.Context) {
	p.mu.Lock()
	taskID := p.taskID
	p.mu.Unlock()
	if taskID == "" {
		return
	}
	_, _ = p.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID})
}

// TaskInfo makes the conversation a task info provider, so a follow-up message
// is addressed to the remote task and its context.
func (p *proxyConversation) TaskInfo() a2a.TaskInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	contextID := p.remoteContext
	if contextID == "" {
		contextID = p.conversation
	}
	return a2a.TaskInfo{TaskID: p.taskID, ContextID: contextID}
}

func (p *proxyConversation) setState(state agent.TaskState) {
	p.mu.Lock()
	p.state = state
	p.mu.Unlock()
}

func (p *proxyConversation) currentState() agent.TaskState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// toA2AParts renders one core message for the wire.
func toA2AParts(msg message.Message) ([]*a2a.Part, error) {
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
			return nil, errdefs.Validationf("manage: part kind %q has no wire form", part.Kind())
		}
	}
	return parts, nil
}

func textOf(msg *a2a.Message) string {
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

func artifactOf(artifact *a2a.Artifact) agent.Artifact {
	out := agent.Artifact{ID: string(artifact.ID), Name: artifact.Name}
	for _, part := range artifact.Parts {
		if part == nil {
			continue
		}
		if url, ok := part.Content.(a2a.URL); ok {
			out.Blob = string(url)
			out.MediaType = part.MediaType
			return out
		}
	}
	return out
}

// fromA2AState translates the protocol's spelling into the internal one.
func fromA2AState(state a2a.TaskState) agent.TaskState {
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
