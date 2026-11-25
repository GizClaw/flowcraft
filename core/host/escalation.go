package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Escalator is the host's second command rule: when a backend refuses a command because it exceeds
// the session's confine, an operator may widen the boundary for that one command, and the host runs
// it in a temporary cell built from the wider grant.
//
// The cell is never widened in place. A boundary is what a session is, so the widened attempt
// happens somewhere else and the session's own cell is untouched: a command that leaves nothing
// behind cannot leave a wider boundary behind either.
type Escalator interface {
	// ApproveWidening answers one request with the grant the temporary cell may run under, or
	// an error that refuses it.
	//
	// An answer is checked by [policy.Grant.Widen] rather than trusted: it must keep every permit
	// the session already had and change nothing but files.write, net and exec.env. An answer that
	// replaces the session's authority is refused, and so is the zero grant.
	//
	// It is called on the turn's goroutine and must return promptly: the model is blocked for
	// as long as it runs, which is what an operator answering a prompt looks like from the
	// model's side.
	ApproveWidening(ctx context.Context, request EscalationRequest) (policy.Grant, error)
}

// EscalationRequest is what an escalator is told: the command that was refused, and the authority
// it needs.
type EscalationRequest struct {
	// Agent and SessionID identify who is asking.
	Agent     string
	SessionID string
	// Command is the command line as words joined by spaces. An operator is being asked to
	// judge a command that already hit its boundary, so the command is the request — and like
	// the approval request it goes no further: the audit record keeps a hash, and no span or
	// record ever carries the text.
	Command string
	// WorkDir is the absolute directory it would run in.
	WorkDir string
	// From is the authority in force for this session. An answer must keep every permit in it and
	// may only widen files.write, net and exec.env — [policy.Grant.Widen] is what enforces that,
	// and [policy.Diff] renders the difference once the operator has proposed one.
	From policy.Grant
}

// WithEscalation installs the operator a refused command may be put to.
//
// It is separate from [WithApproval] because the two answer different questions — whether the
// command may run at all, and whether the boundary may be widened for it — and a process may have
// either without the other. Both are opt-in: with no escalator installed a command outside the
// session's authority stays refused.
func WithEscalation(escalator Escalator) AgentsOption {
	return func(a *Agents) { a.escalator = escalator }
}

// DefaultEscalationTTL is how long a widened cell may live when the process states no preference.
//
// A temporary cell has to end: a widening with no lifetime is a new authority, not one command.
// The value is short because the command it covers is one command, and the caller's own deadline
// still applies underneath.
const DefaultEscalationTTL = 5 * time.Minute

// WithEscalationTTL bounds how long a widened cell may live.
func WithEscalationTTL(ttl time.Duration) AgentsOption {
	return func(a *Agents) {
		if ttl > 0 {
			a.escalationTTL = ttl
		}
	}
}

// The outcomes host.escalations counts, as the series labels them.
const (
	// escalationApproved is a widening an operator agreed to and a temporary cell ran under.
	escalationApproved = "approved"
	// escalationDenied is a widening an operator refused.
	escalationDenied = "denied"
	// escalationUnavailable is a widening that was granted but ran nothing: the temporary cell
	// never started the command, or the process could not build it.
	escalationUnavailable = "unavailable"
	// escalationUnauthorized is an answer that is not a widening of the session's authority, so
	// nothing ran.
	escalationUnauthorized = "unauthorized"
)

// spanEscalated is the trace's record of one widening, carrying the two bind digests and nothing
// else: a span event is read by whoever holds the trace, and the command is not theirs to read.
const spanEscalated = "host.escalated"

// recordEscalation counts one widening attempt by what became of it.
//
// Unavailable is counted because nobody asked about it: a widening the operator granted that then
// ran nothing is the one outcome a reader has to reconstruct from two places.
func recordEscalation(ctx context.Context, agentName, outcome string) {
	meter := telemetry.MeterWithSuffix("host")
	if counter, err := meter.Int64Counter("host.escalations",
		metric.WithDescription("Command widenings by outcome")); err == nil {
		attrs := []attribute.KeyValue{attribute.String("outcome", outcome)}
		if agentName != "" {
			attrs = append(attrs, attribute.String(identity.AttrAgentName, agentName))
		}
		counter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

// escalated records the widening on the turn's span.
//
// The digests are the whole payload: they say which authority was in force and which one the
// command ran under, which is what a trace needs to answer "did this command run outside its
// session". They are also stable, which a path or a command line is not.
func escalated(ctx context.Context, from, to string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.AddEvent(spanEscalated,
		trace.WithAttributes(
			telemetry.String(telemetry.AttrEscalationFromBind, from),
			telemetry.String(telemetry.AttrEscalationToBind, to),
		))
}

// escalationRecord is the payload of the audit record one widening leaves behind.
//
// It carries hashes and digests only. The command is hashed rather than quoted, the authority is
// named by digest, and no path, output or argument appears: the ledger says what was decided, and
// the decision is a fact about authority, not about what was said.
type escalationRecord struct {
	Check       string `json:"check"`
	CommandHash string `json:"cmd_hash"`
	FromBind    string `json:"from_bind"`
	ToBind      string `json:"to_bind"`
	Approver    string `json:"approver"`
	Decision    string `json:"decision"`
	DurationMS  int64  `json:"duration_ms"`
}

// escalationKind is the record's kind in the task's event log.
const escalationKind = "escalation"

// commandHash identifies the command without quoting it: a digest of its words and the directory
// it would have run in.
//
// The words are separated by a NUL so ("a b", "c") and ("a", "b c") do not collide, and the workdir
// is part of the input because "rm x" in two directories is two different commands.
func commandHash(argv []string, workDir string) string {
	sum := sha256.New()
	for _, word := range argv {
		_, _ = sum.Write([]byte(word))
		_, _ = sum.Write([]byte{0})
	}
	_, _ = sum.Write([]byte(workDir))
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

// auditEscalation appends one escalation record to the session's task.
//
// The store's contract is one checkpoint and its facts per save, and there is no "events only"
// form: the record is appended by writing back the projection the task already has, which keeps
// whatever state the turn left there. A task with no checkpoint yet is one the record has nothing
// to attach to, and a store that fails is a second problem the command must not inherit — the
// command already ran, so the record is skipped and said so at debug, never returned.
func (a *Agents) auditEscalation(ctx context.Context, sessionID string, record escalationRecord) {
	if a.store == nil {
		telemetry.Debug(ctx, "host: escalation audit skipped: this process keeps no store",
			telemetry.String(identity.AttrSessionID, sessionID))
		return
	}
	// WithoutCancel: the decision has been made and the command has run. A cancelled turn is
	// exactly when the ledger matters, so the write does not inherit the cancellation.
	auditCtx := context.WithoutCancel(ctx)
	payload, err := json.Marshal(record)
	if err != nil {
		telemetry.WarnErr(ctx, "host: the escalation record could not be encoded", err)
		return
	}
	loaded, err := a.store.LatestForSession(auditCtx, sessionID)
	switch {
	case errors.Is(err, agent.ErrNoCheckpoint):
		telemetry.Debug(ctx, "host: escalation audit skipped: the session has no task yet",
			telemetry.String(identity.AttrSessionID, sessionID))
		return
	case err != nil:
		telemetry.WarnErr(ctx, "host: the escalation record could not be attached to a task", err)
		return
	}
	if _, _, err := a.store.Save(auditCtx, loaded,
		[]agent.EventRecord{{Kind: escalationKind, Payload: payload}}); err != nil {
		telemetry.WarnErr(ctx, "host: the escalation record was not written", err)
	}
}

// widener is one session's escalation state: the authority it runs under and how long a widened
// cell may live.
//
// It is a value the decorator holds per session because the authority is the session's.
type widener struct {
	bind policy.Grant
	ttl  time.Duration
}

// request builds the question: the command, and the authority it needs widened.
func (w *widener) request(agentName, sessionID string, spec sandbox.SessionSpec) EscalationRequest {
	return EscalationRequest{
		Agent:     agentName,
		SessionID: sessionID,
		Command:   strings.Join(spec.Argv, " "),
		WorkDir:   spec.Opts.WorkDir,
		From:      w.bind,
	}
}

// attempt bounds one widened retry by the process's lifetime for a widened cell.
//
// The deadline the command already had is not the bound: a temporary cell has to end whatever the
// command asked for. The caller's own deadline still applies underneath, because shortening what a
// caller asked for is the one direction this layer may move in. The returned cancel belongs to the
// temporary cell — closing the session runs it, and so does a cell that never started a command.
func (w *widener) attempt(ctx context.Context, spec sandbox.SessionSpec) (context.Context, sandbox.SessionSpec, context.CancelFunc) {
	ttl := w.ttl
	if ttl <= 0 {
		return ctx, spec, func() {}
	}
	if spec.Opts.Timeout <= 0 || spec.Opts.Timeout > ttl {
		spec.Opts.Timeout = ttl
	}
	bounded, cancel := context.WithTimeout(ctx, ttl)
	return bounded, spec, cancel
}

// widenerFor is one session's escalation state, or nil when the process installed no escalator.
func (a *Agents) widenerFor(bind policy.Grant) *widener {
	if a.escalator == nil {
		return nil
	}
	ttl := a.escalationTTL
	if ttl <= 0 {
		ttl = DefaultEscalationTTL
	}
	return &widener{bind: bind, ttl: ttl}
}

// auditor writes the escalation records of one session.
func (a *Agents) auditor(sessionID string) func(context.Context, escalationRecord) {
	return func(ctx context.Context, record escalationRecord) {
		a.auditEscalation(ctx, sessionID, record)
	}
}

// describe names the installed escalator for the audit record.
//
// The record's approver is the implementation, not a person: the host is handed a value and a
// grant, and the identity of whoever answered is the escalator's to know. A deployment that needs
// a named operator in the ledger keeps it where the name actually is — in its own escalator's
// records — rather than having the host invent one.
func describe(escalator Escalator) string {
	if escalator == nil {
		return ""
	}
	return fmt.Sprintf("%T", escalator)
}
