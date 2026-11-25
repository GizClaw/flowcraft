package host

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// questionTTL bounds how long a question waits for its answer.
//
// It is the same window as an approval's: a question nobody answered is not a question the session
// should keep acting on, and the turn that asked has been over for a while.
const questionTTL = approvalTTL

// questionsRoute is one session's question surface: where an ask_user question goes, and where its
// answer is kept until the resumed tool call takes it.
//
// A session a caller opened asks the application, through [WithQuestions]; a delegated child asks
// its parent, through the same outcome the approvals use. The route does not know which: it poses
// the question and, when the session has a sink, reports it. For a child the sink is nil and the
// question reaches the parent as the suspension's text, which the delegation already delivers.
type questionsRoute struct {
	mu      sync.Mutex
	session string
	sink    func(ctx context.Context, sessionID string, question message.Content) bool

	posed   *posedQuestion
	replied *message.Content
}

// posedQuestion is what the session asked and has not been answered.
type posedQuestion struct {
	text  message.Content
	asked time.Time
}

// newQuestionsRoute builds the route a session's ask_user calls travel.
func newQuestionsRoute(sessionID string, sink func(context.Context, string, message.Content) bool) *questionsRoute {
	return &questionsRoute{session: sessionID, sink: sink}
}

var _ platform.Questions = (*questionsRoute)(nil)

// Ask records the question and reports the text the turn suspends on, or nil when the session holds
// an answer already.
func (r *questionsRoute) Ask(ctx context.Context, question message.Content) *message.Content {
	r.mu.Lock()
	if r.replied != nil {
		r.mu.Unlock()
		return nil
	}
	if r.posed != nil && time.Since(r.posed.asked) <= questionTTL {
		text := r.posed.text
		r.mu.Unlock()
		return &text
	}
	r.posed = &posedQuestion{text: question, asked: time.Now()}
	r.mu.Unlock()

	recordQuestion(ctx, r.session)
	if r.sink != nil && !r.sink(ctx, r.session, question) {
		telemetry.Warn(ctx, "host: the question outlet refused a question",
			telemetry.String("session.id", r.session))
	}
	text := question
	return &text
}

// Take consumes the answer the session is holding.
func (r *questionsRoute) Take(context.Context) (message.Content, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	replied := r.replied
	r.replied = nil
	r.posed = nil
	answer := replied
	if answer == nil {
		return message.Content{}, false
	}
	return *answer, true
}

// reply records the caller's answer, or reports that nothing was asked.
func (r *questionsRoute) reply(content message.Content) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.posed == nil || time.Since(r.posed.asked) > questionTTL {
		return errdefs.Validationf("host: session %q has no question waiting for an answer", r.session)
	}
	r.replied = &content
	return nil
}

// open reports whether the session is waiting for an answer.
func (r *questionsRoute) open() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.posed != nil
}

// recordQuestion counts one question by the session that asked it.
func recordQuestion(ctx context.Context, sessionID string) {
	telemetry.Debug(ctx, "host: a session asked its user a question",
		telemetry.String("session.id", sessionID))
}

// WithQuestions installs the outlet a session's ask_user calls use.
//
// It is the application's job — a terminal prompt, a chat message, a ticket — for the same reason
// [Approval.Approver] is: the platform knows when to ask, not how the conversation reaches a person.
// Without it, ask_user reports that this deployment has no outlet instead of suspending a turn
// nobody can answer.
func WithQuestions(sink func(ctx context.Context, sessionID string, question message.Content) bool) AgentsOption {
	return func(a *Agents) { a.questions = sink }
}
