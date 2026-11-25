package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/goccy/go-json"
	"go.opentelemetry.io/otel/trace"
)

// delegator is one session's delegation capability: the closure a tool calls when the model asks for
// another agent.
//
// It holds the parent's address and the values a delegation needs from it — the grant that permits
// it and the lineage it extends — never the session itself. A parent that is closed while its child
// still runs is the reason: a pointer would be half a session by then, while an id is something the
// table can answer for, and the delivery of a result goes through that table.
type delegator struct {
	agents *Agents
	// parent is the parent session's id, and agent its deployment.
	parent string
	agent  string
	// grant is the authority the parent held at open: the policy that admits an edge, and the
	// lineage the child extends.
	grant policy.Grant
	chain identity.Chain
	// gate bounds in-flight synchronous edges. It is built from the policy that permits them, so a
	// grant that names no limit gets an unbounded (nil) gate rather than an arbitrary one.
	gate chan struct{}
}

// acquire takes one of the policy's concurrent-edge slots, or refuses.
func (d *delegator) acquire(ctx context.Context) (func(), error) {
	if d.gate == nil {
		return func() {}, nil
	}
	select {
	case d.gate <- struct{}{}:
		return func() { <-d.gate }, nil
	case <-ctx.Done():
		return nil, errdefs.FromContext(ctx.Err())
	}
}

var _ platform.Delegation = (*delegator)(nil)

// Delegate runs one child agent under the parent's delegation policy.
//
// The policy is the parent session's, not the target's: delegation is the one capability that can
// manufacture a run with a new grant, so "may this session hand work to that agent, that way, this
// deep" is answered from the caller's side and nowhere else.
func (d *delegator) Delegate(ctx context.Context, req platform.DelegationRequest) (platform.DelegationOutcome, error) {
	declared := d.grant.Delegation
	target := strings.TrimSpace(req.Target)
	if err := admitDelegation(declared, target, req.Mode, d.chain); err != nil {
		return platform.DelegationOutcome{}, err
	}
	// The input is the model's text, and a delegation is how a session moves it somewhere its own
	// grant does not reach. The cap is the policy's, because how much may be moved is a permission.
	if limit := declared.InputLimit(); len(req.Input) > limit {
		return platform.DelegationOutcome{}, errdefs.Validationf(
			"host: delegation input is %d bytes, and the policy allows %d", len(req.Input), limit)
	}

	// One child per delegation edge: the id is derived so two edges of one chain are distinguishable,
	// and carries a random suffix so an id is never reused — an edge counter restarts with the
	// process, and a restarted parent would otherwise mint the id of a child whose task is still in
	// the store, continuing somebody else's conversation.
	edge, err := newEdgeID(d.agents.nextEdge())
	if err != nil {
		return platform.DelegationOutcome{}, err
	}
	child := d.parent + "/" + target + "-" + edge

	// The first edge of a chain mints its id; every edge below it extends the one it was given.
	// Without an id the lineage cannot be attached at all, which is what made max_depth inert and
	// left chain.id off every span.
	chain := identity.Chain{
		ID:      d.chain.ID,
		Depth:   d.chain.Depth + 1,
		Lineage: append(slices.Clone(d.chain.Lineage), d.parent),
	}
	if chain.ID == "" {
		minted, err := newChainID()
		if err != nil {
			return platform.DelegationOutcome{}, err
		}
		chain.ID = minted
	}
	childGrant, err := d.childAuthority(ctx, target)
	if err != nil {
		return platform.DelegationOutcome{}, err
	}
	if _, err := d.agents.Open(ctx, child, target, childGrant, withChain(chain)); err != nil {
		return platform.DelegationOutcome{}, err
	}

	outcome := platform.DelegationOutcome{
		Session: child,
		Agent:   target,
		Chain:   chain.ID,
		Depth:   chain.Depth,
	}
	if req.Mode == "async" {
		// An async edge is a hand-off: the child is started and the caller gets its address. The run
		// outlives the turn that asked for it, so it gets a root span of its own linked to the asking
		// turn — a child of a span that has already ended would be a trace nobody can read.
		//
		// The goroutine owns the child from here, including closing it: a close registered by the
		// caller would run as this function returned and erase the session before the run it just
		// started, which is what made every async edge a no-op with a "submitted" answer.
		parentSpan := trace.SpanContextFromContext(ctx)
		bgCtx, span := telemetry.TracerWithSuffix("host").Start(
			nestedRunCtx(context.WithoutCancel(ctx)), "host.delegate.async",
			trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: parentSpan}))
		outcome.Status = string(agent.StateSubmitted)
		go func() {
			defer span.End()

			// The child's own events go to the host's sink, and the fold here is what the parent is
			// told when the run ends. A hand-off is the one delegation nobody waits for, so the
			// result is delivered rather than dropped: the parent is told by the first channel that
			// can take it.
			folded := &foldedStream{}
			final := outcome
			sink := func(sinkCtx context.Context, sessionID string, kind HostTurnKind, event agent.Event, runErr error) bool {
				if runErr != nil {
					// Nobody is waiting for this run: the log and the span are what a reader has.
					telemetry.WarnErr(sinkCtx, "host: an async delegation failed", runErr)
				}
				foldDelegated(&final, folded, event)
				if hook := d.agents.hostTurnEvents; hook != nil {
					return hook(sinkCtx, sessionID, kind, event, runErr)
				}
				return true
			}
			run, claimErr := d.agents.turn(bgCtx, child, userMessage(req.Input), turnDelegation, sink)
			if claimErr != nil {
				telemetry.WarnErr(bgCtx, "host: an async delegation could not start", claimErr)
				final.Status = string(agent.StateFailed)
			} else {
				run()
			}
			final.Content = folded.content()
			// WithoutCancel: the close must land even when the asking turn has ended, and the
			// session id is the only thing it needs. A child that paused is kept open — its
			// question is answered later, by the parent, and closing it here would erase the state
			// the question was asked from. It is closed when the answer ends its turn, or by a
			// cancel.
			if final.Status != string(agent.StateInputRequired) {
				_ = d.agents.CloseSession(context.WithoutCancel(bgCtx), child)
			}
			// Detached: the child's turn is over, and the parent's delivery is its own work.
			d.agents.deliver(context.WithoutCancel(bgCtx), d.parent, deliveryMessage(final))
		}()
		return outcome, nil
	}

	// A sync edge is a request: the child exists for as long as this call, and the caller's
	// context is the one that bounds it — except when it paused to ask. A paused child is kept
	// open so the caller can answer it with delegation_answer, which is what makes a sync edge
	// two-phase instead of blocked.
	defer func() {
		if outcome.Status == string(agent.StateInputRequired) {
			return
		}
		_ = d.agents.CloseSession(ctx, child)
	}()

	release, err := d.acquire(ctx)
	if err != nil {
		return outcome, err
	}
	defer release()

	// A sync edge can outlive its parent's patience: the timeout is what stops a child that never
	// finishes from taking the parent's turn with it, and cancelling the child is what stops the
	// child from continuing to run unobserved after the answer was given up on.
	runCtx := ctx
	if timeout := declared.Timeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	folded := &foldedStream{}
	for event, runErr := range d.agents.Run(nestedRunCtx(runCtx), child, userMessage(req.Input)) {
		if runErr != nil {
			if errors.Is(runErr, context.DeadlineExceeded) {
				_ = d.agents.Cancel(context.WithoutCancel(ctx), child, "delegation timed out")
				return outcome, errdefs.Timeoutf(
					"host: delegation to %q exceeded %s", target, declared.Timeout)
			}
			return outcome, runErr
		}
		foldDelegated(&outcome, folded, event)
	}
	outcome.Content = folded.content()
	return outcome, nil
}

// childAuthority resolves what a delegated child may do.
//
// It happens on the trusted side: a delegation must never manufacture authority the parent did not
// hold, so the default is to inherit the parent's grant. An application that needs per-target
// authority installs [WithDelegationGrant], and the answer is taken as given — like an escalator,
// the resolver is host code and attenuation is its job.
func (d *delegator) childAuthority(ctx context.Context, target string) (policy.Grant, error) {
	inherited := d.grant
	resolve := d.agents.delegationGrant
	if resolve == nil {
		return inherited, nil
	}
	return resolve(ctx, d.agent, target, inherited)
}

// admitDelegation answers the three questions the grant asks: which agent, which mode, how deep.
func admitDelegation(d *policy.Delegation, target, mode string, chain identity.Chain) error {
	switch {
	case d == nil || len(d.Targets) == 0:
		return &policy.Denial{Check: policy.CheckDelegation, Reason: "this session may not delegate"}
	case target == "":
		return errdefs.Validationf("host: delegation needs a target agent")
	case !slices.Contains(d.Targets, target):
		return &policy.Denial{Check: policy.CheckDelegation, Reason: fmt.Sprintf(
			"delegation to %q is not in this session's policy (it may reach %s)",
			target, strings.Join(d.Targets, ", "))}
	}
	if d.MaxDepth > 0 && chain.Depth+1 > d.MaxDepth {
		return &policy.Denial{Check: policy.CheckDelegation, Reason: fmt.Sprintf(
			"a delegation %d edges deep is beyond what the policy allows (%d)",
			chain.Depth+1, d.MaxDepth)}
	}
	allowed := make([]string, 0, len(d.Modes))
	for _, permitted := range d.Modes {
		allowed = append(allowed, string(permitted))
	}
	if len(allowed) > 0 && !slices.Contains(allowed, mode) {
		return &policy.Denial{Check: policy.CheckDelegation, Reason: fmt.Sprintf(
			"delegation mode %q is not in this session's policy (it allows %s)",
			mode, strings.Join(allowed, ", "))}
	}
	return nil
}

// The metadata keys a delivered message carries.
//
// The text is what a reader and the model read; these are the facts an application routes and
// groups by. They are attributes rather than new typed fields because a message's metadata is
// exactly "what the transport knew and the model does not", and because a delivery is not a
// protocol: nothing outside this host has to understand it.
const (
	// DeliverySessionKey names the child session the run belonged to.
	DeliverySessionKey = "delegation.session"
	// DeliveryAgentKey names the deployment the child ran.
	DeliveryAgentKey = "delegation.agent"
	// DeliveryChainKey names the delegation chain the run belongs to.
	DeliveryChainKey = "delegation.chain"
	// DeliveryDepthKey is the number of delegation edges above the child.
	DeliveryDepthKey = "delegation.depth"
	// DeliveryStatusKey is the child's task state when the delivery was made.
	DeliveryStatusKey = "delegation.status"
)

// deliveryMessage is what a parent is told when a delegated run finishes.
//
// It is a user turn because that is what a driver's input slot accepts — the conversation gained a
// message, and the platform does not fabricate an assistant turn the model never wrote. The text
// says where it came from so a reader (and the model) can tell it apart from what a person said;
// the metadata carries the same facts for an application that must not parse a sentence.
func deliveryMessage(outcome platform.DelegationOutcome) message.Message {
	var body strings.Builder
	body.WriteString("[delegation] a delegated run finished\n")
	body.WriteString("agent: " + outcome.Agent + "\n")
	body.WriteString("session: " + outcome.Session + "\n")
	body.WriteString("status: " + outcome.Status + "\n")
	body.WriteString("depth: " + strconv.Itoa(outcome.Depth) + "\n")
	if text := outcome.Content.Text(); text != "" {
		body.WriteString("answer:\n" + text + "\n")
	}
	if outcome.Question != nil && outcome.Question.Text() != "" {
		body.WriteString("question:\n" + outcome.Question.Text() + "\n")
	}
	msg := message.NewTextMessage(message.RoleUser, strings.TrimRight(body.String(), "\n"))
	msg.Metadata = &message.Metadata{Attributes: map[string]json.RawMessage{
		DeliverySessionKey: jsonString(outcome.Session),
		DeliveryAgentKey:   jsonString(outcome.Agent),
		DeliveryChainKey:   jsonString(outcome.Chain),
		DeliveryDepthKey:   jsonNumber(outcome.Depth),
		DeliveryStatusKey:  jsonString(outcome.Status),
	}}
	return msg
}

// jsonString encodes one metadata value.
func jsonString(value string) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		// A string cannot fail to marshal; an empty value keeps the key present.
		return json.RawMessage(`""`)
	}
	return encoded
}

// jsonNumber encodes one numeric metadata value.
func jsonNumber(value int) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("0")
	}
	return encoded
}

// foldedStream is what a delegation accumulates while the child streams.
//
// The answer's deltas are kept as they arrive and joined once: concatenating per delta copies the
// whole answer on every event, which is quadratic in the number of deltas — and a delegated edge
// would pay it twice, once in the child's decorator and once here.
type foldedStream struct {
	deltas []string
}

// content materialises the answer once the stream has ended: one allocation, sized exactly,
// instead of a copy per delta.
func (f *foldedStream) content() message.Content {
	if len(f.deltas) == 0 {
		return message.Content{}
	}
	return message.NewTextContent(strings.Join(f.deltas, ""))
}

// foldDelegated keeps what the caller of a delegation needs from the child's stream: its answer, the
// question it paused on, and the terminal state. Everything else belongs to the child's own trace.
func foldDelegated(outcome *platform.DelegationOutcome, folded *foldedStream, event agent.Event) {
	switch typed := event.(type) {
	case agent.Text:
		if typed.Text != "" {
			folded.deltas = append(folded.deltas, typed.Text)
		}
	case agent.Status:
		if typed.State.Terminal() || typed.State == agent.StateInputRequired {
			outcome.Status = string(typed.State)
		}
		if typed.State == agent.StateInputRequired && typed.Reason != "" {
			question := message.NewTextContent(typed.Reason)
			outcome.Question = &question
		}
	}
}

// newEdgeID names one delegation edge: its number within this process, and four random bytes so two
// processes never mint the same id.
func newEdgeID(edge int64) (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("host: mint a delegation edge: %w", err)
	}
	return strconv.FormatInt(edge, 36) + "-" + hex.EncodeToString(suffix[:]), nil
}

// nextEdge numbers delegation edges within this process, so two children of one parent get distinct
// session ids without a clock or a random source in the identity.
func (a *Agents) nextEdge() int64 { return a.edges.Add(1) }

// nestedRunCtx is the context a delegated child runs under: the parent's identity and the
// parent's failure classification are both stripped, because the child attaches its own session
// (which is what carries the chain) and classifies its own failures.
//
// Without the first, `inject.Session` refuses the child's session and the error was swallowed, so
// every span of the child carried the parent's conversation and the parent's bind — a different
// grant. Without the second, the parent's first-wins box recorded the child's model error as the
// parent turn's reason.
func nestedRunCtx(ctx context.Context) context.Context {
	return agent.WithoutFailureBox(identity.Detach(ctx))
}

// newChainID mints the id of a delegation chain's first edge.
func newChainID() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("host: mint a chain id: %w", err)
	}
	return "chain-" + hex.EncodeToString(suffix[:]), nil
}

func userMessage(text string) message.Message {
	return message.NewTextMessage(message.RoleUser, text)
}

// child checks that session names a child of this parent.
//
// The id a delegation mints is derived from the parent, so "may I address this" is a prefix test — and a
// session may address only the edges it opened, never another conversation's child or its own parent.
func (d *delegator) child(session string) error {
	if session == "" {
		return errdefs.Validationf("host: delegation needs a session id")
	}
	if !strings.HasPrefix(session, d.parent+"/") {
		return errdefs.Forbiddenf("host: session %q is not a delegation of this session", session)
	}
	return nil
}

// Status reports where a child stands: the state its checkpoint recorded. The answer and question are
// not here — a process that did not run the child reads its state, not its transcript.
func (d *delegator) Status(ctx context.Context, session string) (platform.DelegationOutcome, error) {
	if err := d.child(session); err != nil {
		return platform.DelegationOutcome{}, err
	}
	if d.agents.store == nil {
		// A host without a store keeps nothing to ask about: the child's state exists only in
		// its driver, which this process does not hold.
		return platform.DelegationOutcome{}, errdefs.NotAvailablef(
			"host: delegation status needs a checkpoint store")
	}
	checkpoint, err := d.agents.store.Load(ctx, session)
	if err != nil {
		return platform.DelegationOutcome{}, err
	}
	return platform.DelegationOutcome{
		Status:  string(checkpoint.Task.State),
		Session: session,
	}, nil
}

// Answer continues a paused child and folds what it produced next.
//
// Two kinds of pause are answered here. An approval question takes a decision, which is recorded
// against the command it was asked about and then applied by the child's own pre-flight; anything
// else takes the caller's text. Either way the child resumes as a turn of its own, with what the
// caller decided as the message it continues from — the child's model sees the decision in its
// conversation, which is what lets it retry a granted command or plan around a refused one.
func (d *delegator) Answer(ctx context.Context, session string, answer platform.DelegationAnswer,
) (platform.DelegationOutcome, error) {
	if err := d.child(session); err != nil {
		return platform.DelegationOutcome{}, err
	}
	child, ok := d.agents.Session(session)
	if !ok {
		return platform.DelegationOutcome{}, errdefs.NotAvailablef(
			"host: delegation %q is not open in this process", session)
	}
	text, err := child.answer(ctx, answer)
	if err != nil {
		return platform.DelegationOutcome{}, err
	}
	outcome := platform.DelegationOutcome{Session: session, Agent: child.agent, Depth: child.chain.Depth, Chain: child.chain.ID}
	folded := &foldedStream{}
	// The child's own identity and failure box, as on the other two run sites: the caller's context
	// carries the parent's session and turn, and a resumed child must not appear as the parent.
	for event, runErr := range d.agents.Resume(nestedRunCtx(ctx), session, message.NewTextContent(text)) {
		if runErr != nil {
			// A failed resume ends the child's work as surely as a terminal status does; leaving it
			// open would keep a cell for a conversation nobody will continue.
			_ = d.agents.CloseSession(context.WithoutCancel(ctx), session)
			return outcome, runErr
		}
		foldDelegated(&outcome, folded, event)
	}
	outcome.Content = folded.content()
	// A child that finished its work is closed; one that paused again stays open for the next
	// answer. Closing a paused child here would erase the state its question was asked from.
	if outcome.Status != string(agent.StateInputRequired) {
		_ = d.agents.CloseSession(context.WithoutCancel(ctx), session)
	}
	return outcome, nil
}

// answer turns a delegation answer into the text the child continues from.
func (s *Session) answer(ctx context.Context, answer platform.DelegationAnswer) (string, error) {
	if answer.Decision != "" {
		if s.approvals == nil {
			return "", errdefs.Validationf(
				"host: %q was not asked for an approval", s.id)
		}
		question, err := s.approvals.decide(ctx, answer.Decision, answer.Text)
		if err != nil {
			return "", err
		}
		command := strings.Join(question.request.Argv, " ")
		switch answer.Decision {
		case platform.DecisionApprove:
			recordDelegationDecision(ctx, s.agent, platform.DecisionApprove)
			return "[approval] the parent approved this command; run it again:\n" + command, nil
		case platform.DecisionEscalate:
			recordDelegationDecision(ctx, s.agent, platform.DecisionEscalate)
		case platform.DecisionDeny:
			recordDelegationDecision(ctx, s.agent, platform.DecisionDeny)
		}
		if granted, ok := s.approvals.lookup(question.digest); ok && granted.approve {
			return "[approval] the deployment's approver granted this command; run it again:\n" + command, nil
		}
		return "[approval] this command was refused and the refusal is final; do not retry it and do " +
			"not ask for an escalation:\n" + command, nil
	}
	if strings.TrimSpace(answer.Text) == "" {
		return "", errdefs.Validationf(
			"host: an answer needs a decision or a reply")
	}
	if s.approvals != nil && s.approvals.open() {
		// Free text cannot answer "may this command run": the child's tool node is suspended on a
		// decision, and a sentence would resume it with nothing decided.
		return "", errdefs.Validationf(
			"host: %q is waiting for a decision: approve, deny or escalate", s.id)
	}
	// A question that is not an approval takes the caller's text: the resumed tool call reads it
	// back through the question route.
	if s.questions != nil && s.questions.open() {
		if err := s.questions.reply(message.NewTextContent(answer.Text)); err != nil {
			return "", err
		}
	}
	return answer.Text, nil
}

// Cancel stops a child.
//
// A child that paused for an answer has no run in flight, so there is nothing to interrupt: the
// cancel is what closes it. Reporting "the session is idle" and leaving it open would leave a cell,
// a driver and a boundary alive for a conversation the parent gave up on.
func (d *delegator) Cancel(ctx context.Context, session, reason string) error {
	if err := d.child(session); err != nil {
		return err
	}
	if err := d.agents.Cancel(ctx, session, reason); err != nil &&
		!errors.Is(err, agent.ErrSessionIdle) && !errdefs.IsNotFound(err) {
		return err
	}
	if _, open := d.agents.Session(session); !open {
		return nil
	}
	return d.agents.CloseSession(context.WithoutCancel(ctx), session)
}

// WithDelegationGrant installs the rule that decides what a delegated child may do.
//
// The default — no resolver — is inheritance: the child runs under the parent's grant. That is
// the only rule that cannot escalate, because a delegation is the one capability that can
// manufacture a new run with a new authority. An application that needs per-target authority
// (a reviewer that may only read, a seat that may only write its own notes) installs this and
// returns something else. The host does not check the answer: like an escalator, the resolver is
// trusted host code and attenuation is its job.
func WithDelegationGrant(resolve func(ctx context.Context, parent, target string, authority policy.Grant) (policy.Grant, error)) AgentsOption {
	return func(a *Agents) { a.delegationGrant = resolve }
}
