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
	"go.opentelemetry.io/otel/trace"
)

// delegator is one session's delegation capability: the closure a tool calls when the model asks for
// another agent.
//
// It holds the parent session's identity rather than the session itself, because what a delegation
// needs from its parent is the grant that permits it and the lineage it extends — not the parent's
// driver, its cell or its binding.
type delegator struct {
	agents *Agents
	parent *Session
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
	parent := d.parent.lineage()
	target := strings.TrimSpace(req.Target)
	if err := admitDelegation(parent.policy, target, req.Mode, parent.chain); err != nil {
		return platform.DelegationOutcome{}, err
	}
	// The input is the model's text, and a delegation is how a session moves it somewhere its own
	// grant does not reach. The cap is the policy's, because how much may be moved is a permission.
	if limit := parent.policy.InputLimit(); len(req.Input) > limit {
		return platform.DelegationOutcome{}, errdefs.Validationf(
			"host: delegation input is %d bytes, and the policy allows %d", len(req.Input), limit)
	}

	// One child per delegation edge: the id is derived so two edges of one chain are distinguishable,
	// and so a resumed parent's next edge does not collide with the one before it.
	child := d.parent.id + "/" + target + "-" + strconv.FormatInt(d.agents.nextEdge(), 36)

	// The first edge of a chain mints its id; every edge below it extends the one it was given.
	// Without an id the lineage cannot be attached at all, which is what made max_depth inert and
	// left chain.id off every span.
	chain := identity.Chain{ID: parent.chain.ID, Depth: parent.chain.Depth + 1}
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
	if _, err := d.agents.Open(ctx, child, target, childGrant, WithChain(chain)); err != nil {
		return platform.DelegationOutcome{}, err
	}

	outcome := platform.DelegationOutcome{
		Session: child,
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
			// WithoutCancel: the close must land even when the asking turn has ended, and the
			// session id is the only thing it needs.
			defer func() { _ = d.agents.CloseSession(context.WithoutCancel(bgCtx), child) }()
			for _, runErr := range d.agents.Run(bgCtx, child, userMessage(req.Input)) {
				if runErr != nil {
					// Nobody is waiting for this run: the log and the span are what a reader has.
					telemetry.WarnErr(bgCtx, "host: an async delegation failed", runErr)
				}
			}
		}()
		return outcome, nil
	}

	// A sync edge is a request: the child exists for as long as this call, and the caller's
	// context is the one that bounds it.
	defer func() { _ = d.agents.CloseSession(ctx, child) }()

	release, err := d.acquire(ctx)
	if err != nil {
		return outcome, err
	}
	defer release()

	// A sync edge can outlive its parent's patience: the timeout is what stops a child that never
	// finishes from taking the parent's turn with it, and cancelling the child is what stops the
	// child from continuing to run unobserved after the answer was given up on.
	runCtx := ctx
	if timeout := parent.policy.Timeout.Std(); timeout > 0 {
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
					"host: delegation to %q exceeded %s", target, parent.policy.Timeout)
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
	inherited := d.parent.lineage().grant
	resolve := d.agents.delegationGrant
	if resolve == nil {
		return inherited, nil
	}
	return resolve(ctx, d.parent.agent, target, inherited)
}

// lineageView is what a delegation needs from its parent: the policy that permits it and the chain it
// extends. It is a value, so the child cannot reach the parent's driver or cell through it.
type lineageView struct {
	policy *policy.Delegation
	grant  policy.Grant
	chain  identity.Chain
}

// lineage is the parent's delegation view. The chain is read from the identity the turn attached, so a
// resumed process continues the chain it recorded rather than starting a new one at depth zero.
func (s *Session) lineage() lineageView {
	return lineageView{policy: s.grant.Delegation, grant: s.grant, chain: s.chain}
}

// admitDelegation answers the three questions the grant asks: which agent, which mode, how deep.
func admitDelegation(d *policy.Delegation, target, mode string, chain identity.Chain) error {
	switch {
	case d == nil || len(d.Targets) == 0:
		return errdefs.Forbiddenf("host: this session may not delegate")
	case target == "":
		return errdefs.Validationf("host: delegation needs a target agent")
	case !slices.Contains(d.Targets, target):
		return errdefs.Forbiddenf(
			"host: delegation to %q is not in this session's policy (it may reach %s)",
			target, strings.Join(d.Targets, ", "))
	}
	if d.MaxDepth > 0 && chain.Depth+1 > d.MaxDepth {
		return errdefs.Forbiddenf(
			"host: delegation would be %d edges deep, and the policy allows %d", chain.Depth+1, d.MaxDepth)
	}
	allowed := make([]string, 0, len(d.Modes))
	for _, permitted := range d.Modes {
		allowed = append(allowed, string(permitted))
	}
	if len(allowed) > 0 && !slices.Contains(allowed, mode) {
		return errdefs.Forbiddenf(
			"host: delegation mode %q is not in this session's policy (it allows %s)",
			mode, strings.Join(allowed, ", "))
	}
	return nil
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
	if !strings.HasPrefix(session, d.parent.id+"/") {
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
func (d *delegator) Answer(ctx context.Context, session, answer string) (platform.DelegationOutcome, error) {
	if err := d.child(session); err != nil {
		return platform.DelegationOutcome{}, err
	}
	outcome := platform.DelegationOutcome{Session: session}
	folded := &foldedStream{}
	for event, err := range d.agents.Run(ctx, session, userMessage(answer)) {
		if err != nil {
			return outcome, err
		}
		foldDelegated(&outcome, folded, event)
	}
	outcome.Content = folded.content()
	return outcome, nil
}

// Cancel stops a child.
func (d *delegator) Cancel(ctx context.Context, session, reason string) error {
	if err := d.child(session); err != nil {
		return err
	}
	return d.agents.Cancel(ctx, session, reason)
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
