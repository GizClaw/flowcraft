package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
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
// Both sides are keyed by the question's digest, not by arrival order: the resumed call asks its
// pre-flight again, so the only thing that identifies "the question I posed" is its text. A reply
// therefore reaches the call that asked it and no other.
//
// A session a caller opened asks the application, through [WithQuestions]; a delegated child asks
// its parent, through the same outcome the approvals use. The route does not know which: it poses
// the question and, when the session has a sink, reports it. For a child the sink is nil and the
// question reaches the parent as the suspension's text, which the delegation already delivers.
type questionsRoute struct {
	mu      sync.Mutex
	session string
	sink    func(ctx context.Context, sessionID string, question message.Content) bool

	posed   map[string]*posedQuestion
	replied map[string]message.Content
}

// posedQuestion is what the session asked and has not been answered.
type posedQuestion struct {
	text  message.Content
	asked time.Time
}

// newQuestionsRoute builds the route a session's ask_user calls travel.
func newQuestionsRoute(sessionID string, sink func(context.Context, string, message.Content) bool) *questionsRoute {
	return &questionsRoute{
		session: sessionID,
		sink:    sink,
		posed:   map[string]*posedQuestion{},
		replied: map[string]message.Content{},
	}
}

var _ platform.Questions = (*questionsRoute)(nil)

// Ask records the question and reports the text the turn suspends on, or nil when the session holds
// an answer to this question already.
func (r *questionsRoute) Ask(ctx context.Context, question message.Content) *message.Content {
	digest := questionDigest(question.Text())
	r.mu.Lock()
	if _, answered := r.replied[digest]; answered {
		r.mu.Unlock()
		return nil
	}
	if posed, ok := r.posed[digest]; ok && time.Since(posed.asked) <= questionTTL {
		text := posed.text
		r.mu.Unlock()
		return &text
	}
	r.posed[digest] = &posedQuestion{text: question, asked: time.Now()}
	r.mu.Unlock()

	recordQuestion(ctx, r.session)
	if r.sink != nil && !r.sink(ctx, r.session, question) {
		telemetry.Warn(ctx, "host: the question outlet refused a question",
			telemetry.String("session.id", r.session))
	}
	text := question
	return &text
}

// Take consumes the answer to question, once.
func (r *questionsRoute) Take(_ context.Context, question message.Content) (message.Content, bool) {
	digest := questionDigest(question.Text())
	r.mu.Lock()
	defer r.mu.Unlock()
	answer, ok := r.replied[digest]
	if !ok {
		return message.Content{}, false
	}
	delete(r.replied, digest)
	delete(r.posed, digest)
	return answer, true
}

// reply records the caller's answer against the question that is open.
//
// At most one question is open per session — a step suspends one node — so the answer has no
// ambiguity here; it becomes ambiguous only if it is still buffered after that question is gone,
// which is why the entry is keyed and expires.
//
// An expired question is not an error the caller can act on: the answer is dropped, the paused
// session resumes, and the pre-flight asks again. Reporting an error instead left the session paused
// with an answer nobody would take, so it could neither resume nor run.
func (r *questionsRoute) reply(content message.Content) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	digest, posed, expired := "", (*posedQuestion)(nil), false
	for key, entry := range r.posed {
		if time.Since(entry.asked) > questionTTL {
			delete(r.posed, key)
			expired = true
			continue
		}
		digest, posed = key, entry
		break
	}
	if posed == nil {
		if expired {
			// The question lapsed while the answer was on its way: drop the answer and let the
			// caller resume, which re-poses it. A refusal here wedged the session instead.
			return nil
		}
		if len(r.replied) > 0 {
			// Never overwrite an answer nobody has taken: that is a caller answering twice, and the
			// first answer is the one the question was asked for.
			return errdefs.Conflictf("host: session %q was already answered", r.session)
		}
		return errdefs.Validationf("host: session %q has no question waiting for an answer", r.session)
	}
	r.replied[digest] = content
	return nil
}

// open reports whether the session is waiting for an answer to a live question.
func (r *questionsRoute) open() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, posed := range r.posed {
		if time.Since(posed.asked) <= questionTTL {
			return true
		}
	}
	return false
}

// questionDigest names a question by its text, with a separator a sentence cannot forge.
func questionDigest(text string) string {
	sum := sha256.New()
	sum.Write([]byte(strconv.Itoa(len(text))))
	sum.Write([]byte{0})
	sum.Write([]byte(text))
	return hex.EncodeToString(sum.Sum(nil))
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
