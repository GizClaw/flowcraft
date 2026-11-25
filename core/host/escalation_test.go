package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/agenttest"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/paths"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/goccy/go-json"
)

// widening is the backend side of an escalation: an instance that refuses a command naming a path
// outside the confine it was built for — which is what a backend reports with sandbox.Deniedf —
// and records the confine of every instance, so a test can see that the temporary cell really was
// wider than the session's own.
type widening struct {
	mu        sync.Mutex
	instances []*widened
	started   [][]string
	// refuseEveryCommand is the widened cell failing as well: the one retry is all there is.
	refuseEveryCommand bool
	// panicWhenWider panics while starting in a widened cell, which must still be released.
	panicWhenWider bool
}

// widened is one backend instance: the confine it enforces and its own closure count.
type widened struct {
	owner   *widening
	confine sandbox.Confine
	closed  atomic.Int32
}

func (w *widening) factory(confine sandbox.Confine) (sandbox.Runner, error) {
	instance := &widened{owner: w, confine: confine}
	w.mu.Lock()
	w.instances = append(w.instances, instance)
	w.mu.Unlock()
	return instance, nil
}

func (w *widening) builds() []*widened {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*widened(nil), w.instances...)
}

func (w *widening) commands() [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]string(nil), w.started...)
}

func (w *widening) Close() error { return nil }

func (w *widening) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{FilesystemBounds: true}}
}

func (*widening) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }

func (*widening) Terminate(context.Context, string) error { return nil }

func (b *widened) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{FilesystemBounds: true}}
}

func (*widened) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }

func (*widened) Terminate(context.Context, string) error { return nil }

func (b *widened) Close() error {
	b.closed.Add(1)
	return nil
}

// Start refuses a command whose target sits outside the instance's write set.
func (b *widened) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if b.owner.panicWhenWider && len(b.confine.Files.Write) > 1 {
		panic("the widened backend exploded while starting")
	}
	target := ""
	if len(spec.Argv) > 0 {
		target = spec.Argv[len(spec.Argv)-1]
	}
	if b.owner.refuseEveryCommand || !writableUnder(b.confine.Files.Write, target) {
		return nil, sandbox.Deniedf("write %q is outside this session's confine", target)
	}
	b.owner.mu.Lock()
	b.owner.started = append(b.owner.started, spec.Argv)
	b.owner.mu.Unlock()
	return &idleSession{id: spec.ID}, nil
}

// writableUnder is the backend's own check, spelled with the same containment the policy uses.
func writableUnder(write []string, target string) bool {
	for _, root := range write {
		if paths.Within(root, target) {
			return true
		}
	}
	return false
}

// idleSession is a started command that does nothing: these tests are about which boundary it
// started in, not about what it printed.
type idleSession struct{ id string }

func (s *idleSession) ID() string { return s.id }

func (*idleSession) PID() int { return 0 }

func (*idleSession) Read(context.Context, int64, int) (sandbox.SessionOutput, error) {
	return sandbox.SessionOutput{EOF: true}, nil
}

func (*idleSession) Write(context.Context, []byte) error { return nil }

func (*idleSession) CloseInput() error { return nil }

func (*idleSession) Resize(context.Context, int, int) error { return nil }

func (*idleSession) Signal(context.Context, sandbox.SessionSignal) error { return nil }

func (*idleSession) Terminate(context.Context) error { return nil }

func (*idleSession) Wait(context.Context) (sandbox.SessionExit, error) {
	return sandbox.SessionExit{Code: 0}, nil
}

func (*idleSession) Watch(context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("sandbox: no event stream")
}

func (*idleSession) Close() error { return nil }

func (*idleSession) Capabilities() sandbox.SessionCapabilities { return sandbox.SessionCapabilities{} }

// operator stands in for whoever answers a widening request.
type operator struct {
	mu     sync.Mutex
	asked  []EscalationRequest
	answer func(EscalationRequest) (policy.Grant, error)
	// widen is what an unconditional yes adds to the session's write set. The answer is a widening
	// of what the session had, never a replacement: [policy.Grant.Widen] is what checks that.
	widen []string
}

func (o *operator) ApproveWidening(_ context.Context, request EscalationRequest) (policy.Grant, error) {
	o.mu.Lock()
	o.asked = append(o.asked, request)
	o.mu.Unlock()
	if o.answer != nil {
		return o.answer(request)
	}
	widened := request.From
	widened.Files.Write = append(append([]string(nil), request.From.Files.Write...), o.widen...)
	return widened, nil
}

func (o *operator) questions() []EscalationRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]EscalationRequest(nil), o.asked...)
}

// escalationGrant is a session authority that may write one directory, and the second directory an
// operator may widen it to: the shape the whole feature exists for.
func escalationGrant(t *testing.T, sandboxName string) (policy.Grant, string) {
	t.Helper()
	root := t.TempDir()
	out, tmp := filepath.Join(root, "out"), filepath.Join(root, "tmp")
	for _, dir := range []string{out, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	grant := permissiveGrant(sandboxName)
	grant.Files = policy.Files{Read: []string{root}, Write: []string{out}}
	return grant, tmp
}

// escalatingHost opens one session under grant, over a backend the test controls, and hands back
// the process capability the host decorated.
func escalatingHost(t *testing.T, grant policy.Grant, opts ...AgentsOption) (sandbox.SessionStarter, *widening, *agenttest.MemStore) {
	t.Helper()
	backend := &widening{}
	res, err := Open(t.Context(), deploy.ProcessPlan{Backends: []deploy.PlannedBackend{
		boundBackend("local", &boundary{log: &journal{}, filesystemBounds: true}),
	}}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	res.backends["local"] = backend.factory

	store := agenttest.NewMemStore()
	agents := NewAgents(res, append([]AgentsOption{WithStore(store)}, opts...)...)
	dep, _ := sessionDeployment(t, "referee", grant, &journal{}, &boundary{log: &journal{}, filesystemBounds: true})
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, err := agents.Open(t.Context(), "s1", "referee", grant)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	starter, ok := platform.ProcessSlot.Get(session.Binding().Caps)
	if !ok || starter == nil {
		t.Fatal("the session has no process capability")
	}
	// A task the escalation record can be attached to: the host writes the record through the
	// session's newest checkpoint, and a session that has not run yet has none.
	if _, _, err := store.Save(t.Context(), agent.Checkpoint{
		Task: agent.Task{ID: "t1", SessionID: "s1", State: agent.StateWorking},
	}, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return starter, backend, store
}

// command is one start request aimed at target, which is what decides whether the confine allows
// it.
func command(target string) sandbox.SessionSpec {
	return sandbox.SessionSpec{ID: "cmd", Argv: []string{"touch", target}, Opts: sandbox.ExecOptions{WorkDir: filepath.Dir(target)}}
}

// records returns the session's escalation records.
func records(t *testing.T, store *agenttest.MemStore) []agent.EventRecord {
	t.Helper()
	events, err := store.Events(t.Context(), "t1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var kept []agent.EventRecord
	for _, event := range events {
		if event.Kind == escalationKind {
			kept = append(kept, event)
		}
	}
	return kept
}

// TestAnApprovedWideningRunsInATemporaryCell is the whole feature: a command the confine refuses is
// offered to an operator, and what they grant is a second cell the command runs in — closed, and
// described in the ledger, when it is done.
func TestAnApprovedWideningRunsInATemporaryCell(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	grant, tmp := escalationGrant(t, "local")
	out := grant.Files.Write[0]
	who := &operator{widen: []string{tmp}}
	starter, backend, store := escalatingHost(t, grant, WithEscalation(who))

	target := filepath.Join(tmp, "scratch")
	running, err := starter.Start(t.Context(), command(target))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The question names the command and the authority in force, and nothing about the session's
	// internals.
	asked := who.questions()
	if len(asked) != 1 {
		t.Fatalf("asked = %d, want one question", len(asked))
	}
	if asked[0].Agent != "referee" || asked[0].SessionID != "s1" {
		t.Fatalf("request = %+v, want the session's identity", asked[0])
	}
	if !strings.Contains(asked[0].Command, target) || asked[0].WorkDir != filepath.Dir(target) {
		t.Fatalf("request = %+v, want the command that was refused", asked[0])
	}
	if got := asked[0].From.Files.Write; len(got) != 1 || got[0] != out {
		t.Fatalf("from = %v, want the session's own write set", got)
	}

	// Two cells were built: the session's, and the widened one the command actually ran in.
	built := backend.builds()
	if len(built) != 2 {
		t.Fatalf("built %d cells, want the session's and the widened one", len(built))
	}
	if got := built[1].confine.Files.Write; len(got) != 2 || got[1] != tmp {
		t.Fatalf("the widened cell confined writes to %v, want the approved set", got)
	}
	if commands := backend.commands(); len(commands) != 1 {
		t.Fatalf("ran %d commands, want the one retry", len(commands))
	}

	// The ledger says a widening happened, by digest and with a command hash — never the command.
	kept := records(t, store)
	if len(kept) != 1 {
		t.Fatalf("records = %d, want one escalation record", len(kept))
	}
	payload := string(kept[0].Payload)
	for _, field := range []string{`"check":"escalation"`, `"decision":"approved"`, `"cmd_hash":"sha256:`, `"from_bind":"`, `"to_bind":"`} {
		if !strings.Contains(payload, field) {
			t.Fatalf("payload = %s, want it to carry %s", payload, field)
		}
	}
	from, to := digestField(t, payload, "from_bind"), digestField(t, payload, "to_bind")
	if from == to {
		t.Fatalf("from_bind and to_bind are both %q: the record does not show a widening", from)
	}
	if strings.Contains(payload, target) || strings.Contains(payload, "touch") {
		t.Fatalf("payload = %s, want no command text and no path", payload)
	}

	counted(t, recorder, escalationApproved, 1)
	counted(t, recorder, escalationDenied, 0)

	// Closing the command closes the temporary cell — the widened boundary does not outlive it.
	if err := running.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := built[1].closed.Load(); got != 1 {
		t.Fatalf("the widened cell was closed %d times, want once", got)
	}
	if got := built[0].closed.Load(); got != 0 {
		t.Fatal("the session's own cell was closed with the command")
	}
	// Closing twice is a no-op, so an error path may close what it was handed.
	if err := running.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := built[1].closed.Load(); got != 1 {
		t.Fatalf("the widened cell was closed %d times, want once", got)
	}
}

// TestARefusedWideningLeavesTheOriginalRefusal is the fail-closed direction: the operator said no,
// so the command did not run, the session's cell is untouched and nothing claims otherwise.
func TestARefusedWideningLeavesTheOriginalRefusal(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	grant, tmp := escalationGrant(t, "local")
	who := &operator{answer: func(EscalationRequest) (policy.Grant, error) {
		return policy.Grant{}, &policy.Denial{Check: policy.CheckApproval, Reason: "operator said no"}
	}}
	starter, backend, store := escalatingHost(t, grant, WithEscalation(who))

	_, err := starter.Start(t.Context(), command(filepath.Join(tmp, "scratch")))
	if !errors.Is(err, sandbox.ErrConfineDenied) {
		t.Fatalf("err = %v, want the confine refusal the backend produced", err)
	}
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want the policy classification to survive", err)
	}
	if len(who.questions()) != 1 {
		t.Fatalf("asked = %d, want one question", len(who.questions()))
	}
	if got := len(backend.builds()); got != 1 {
		t.Fatalf("built %d cells, want only the session's", got)
	}
	if kept := records(t, store); len(kept) != 0 {
		t.Fatalf("records = %d, want none: nothing was widened", len(kept))
	}
	counted(t, recorder, escalationDenied, 1)
	counted(t, recorder, escalationApproved, 0)
}

// TestNoEscalatorLeavesTheRefusal is the opt-in default: a process that installed no operator has
// offered nothing, so a command outside the session's authority stays refused rather than failing
// with a second error class.
func TestNoEscalatorLeavesTheRefusal(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	grant, tmp := escalationGrant(t, "local")
	starter, backend, store := escalatingHost(t, grant)

	_, err := starter.Start(t.Context(), command(filepath.Join(tmp, "scratch")))
	if !errors.Is(err, sandbox.ErrConfineDenied) {
		t.Fatalf("err = %v, want the confine refusal", err)
	}
	if got := len(backend.builds()); got != 1 {
		t.Fatalf("built %d cells, want only the session's", got)
	}
	if kept := records(t, store); len(kept) != 0 {
		t.Fatalf("records = %d, want none", len(kept))
	}
	counted(t, recorder, escalationUnavailable, 0)
}

// TestAWideningIsRetriedOnce pins the retry: the widened cell failing again is the answer, so the
// second refusal is returned and nobody is asked a second time.
func TestAWideningIsRetriedOnce(t *testing.T) {
	grant, tmp := escalationGrant(t, "local")
	who := &operator{widen: []string{tmp}}
	starter, backend, _ := escalatingHost(t, grant, WithEscalation(who))
	backend.refuseEveryCommand = true

	_, err := starter.Start(t.Context(), command(filepath.Join(tmp, "scratch")))
	if !errors.Is(err, sandbox.ErrConfineDenied) {
		t.Fatalf("err = %v, want the widened cell's refusal", err)
	}
	if got := len(who.questions()); got != 1 {
		t.Fatalf("asked = %d, want one: the retry is never a new question", got)
	}
	if got := len(backend.builds()); got != 2 {
		t.Fatalf("built %d cells, want one attempt in a second cell", got)
	}
}

// TestAnAnswerThatIsNotAWideningIsRefused is what replaced the declared ceiling: the answer has to
// keep every permit the session had, so an operator that hands back a different authority — here
// one that drops the root the session could already write — is not trusted.
func TestAnAnswerThatIsNotAWideningIsRefused(t *testing.T) {
	recorder := telemetrytest.Meters(t)
	grant, tmp := escalationGrant(t, "local")
	who := &operator{answer: func(request EscalationRequest) (policy.Grant, error) {
		replaced := request.From
		replaced.Files.Write = []string{tmp}
		return replaced, nil
	}}
	starter, backend, store := escalatingHost(t, grant, WithEscalation(who))

	_, err := starter.Start(t.Context(), command(filepath.Join(tmp, "scratch")))
	if !errors.Is(err, sandbox.ErrConfineDenied) {
		t.Fatalf("err = %v, want the original refusal", err)
	}
	if got := len(backend.builds()); got != 1 {
		t.Fatalf("built %d cells, want only the session's: a replacement is not a widening", got)
	}
	if kept := records(t, store); len(kept) != 0 {
		t.Fatalf("records = %d, want none", len(kept))
	}
	counted(t, recorder, escalationUnauthorized, 1)
}

// TestAPanickingCommandStillClosesTheTemporaryCell is the cleanup promise: a backend that explodes
// while starting leaves no cell behind, because the widened boundary is the one thing this feature
// must never leak.
func TestAPanickingCommandStillClosesTheTemporaryCell(t *testing.T) {
	grant, tmp := escalationGrant(t, "local")
	who := &operator{widen: []string{tmp}}
	starter, backend, _ := escalatingHost(t, grant, WithEscalation(who))
	backend.panicWhenWider = true

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("the backend's panic was swallowed")
			}
		}()
		_, _ = starter.Start(t.Context(), command(filepath.Join(tmp, "scratch")))
	}()

	built := backend.builds()
	if len(built) != 2 {
		t.Fatalf("built %d cells, want the session's and the widened one", len(built))
	}
	if got := built[1].closed.Load(); got != 1 {
		t.Fatalf("the widened cell was closed %d times after a panic, want once", got)
	}
}

// TestTheEscalationTTLBoundsTheWidenedCell is the one lifetime rule that stayed with the host: a
// temporary cell has to end, so a widened attempt is bounded even when the command asked for longer.
// The caller's own shorter deadline still wins.
func TestTheEscalationTTLBoundsTheWidenedCell(t *testing.T) {
	agents := NewAgents(&Resource{}, WithEscalation(&operator{}))
	who := agents.widenerFor(policy.Grant{})
	ctx, spec, cancel := who.attempt(t.Context(), sandbox.SessionSpec{Opts: sandbox.ExecOptions{Timeout: time.Hour}})
	defer cancel()
	if spec.Opts.Timeout != DefaultEscalationTTL {
		t.Fatalf("attempt timeout = %v, want %v", spec.Opts.Timeout, DefaultEscalationTTL)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > DefaultEscalationTTL {
		t.Fatalf("attempt deadline = %v (ok=%v), want at most the default TTL", deadline, ok)
	}

	shorter := 10 * time.Second
	_, spec, cancel = who.attempt(t.Context(), sandbox.SessionSpec{Opts: sandbox.ExecOptions{Timeout: shorter}})
	defer cancel()
	if spec.Opts.Timeout != shorter {
		t.Fatalf("attempt timeout = %v, want the caller's shorter %v", spec.Opts.Timeout, shorter)
	}

	tuned := NewAgents(&Resource{}, WithEscalation(&operator{}), WithEscalationTTL(time.Minute))
	_, spec, cancel = tuned.widenerFor(policy.Grant{}).attempt(t.Context(), sandbox.SessionSpec{})
	defer cancel()
	if spec.Opts.Timeout != time.Minute {
		t.Fatalf("attempt timeout = %v, want the configured ttl", spec.Opts.Timeout)
	}
}

// counted asserts how many widenings one outcome recorded.
func counted(t *testing.T, recorder *telemetrytest.MetricRecorder, outcome string, want float64) {
	t.Helper()
	got, present := recorder.Collect(t).SumWhere("host.escalations", map[string]string{"outcome": outcome})
	if want == 0 {
		if present && got != 0 {
			t.Fatalf("host.escalations{outcome=%q} = %v, want none", outcome, got)
		}
		return
	}
	if !present || got != want {
		t.Fatalf("host.escalations{outcome=%q} = %v (present %v), want %v", outcome, got, present, want)
	}
}

// digestField reads one string field out of a record payload, which is how a test compares two
// digests without knowing the payload's shape.
func digestField(t *testing.T, payload, field string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload %s: %v", payload, err)
	}
	value, _ := decoded[field].(string)
	if value == "" {
		t.Fatalf("payload %s carries no %q", payload, field)
	}
	return value
}
