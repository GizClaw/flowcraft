package host

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool/platform"
)

// boundary is a backend instance: it records its own closure and whether it claims to bound writes.
//
// closed is atomic because one instance stands in for every session a test opens, and a
// session that fails to open closes the cell it just built: the concurrent-opens test drives
// eight of those at once, and a plain counter there is a race the type system does not catch.
type boundary struct {
	log              *journal
	filesystemBounds bool
	buildErr         error
	closed           atomic.Int32
}

func (b *boundary) Close() error {
	b.closed.Add(1)
	b.log.add("close:sandbox")
	return nil
}

func (b *boundary) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{FilesystemBounds: b.filesystemBounds}}
}

func (*boundary) Start(context.Context, sandbox.SessionSpec) (sandbox.Session, error) {
	return nil, errors.New("not used")
}

func (*boundary) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }

func (*boundary) Terminate(context.Context, string) error { return nil }

// boundBackend is a deployment whose backend factory returns b, or fails.
func boundBackend(name string, b *boundary) deploy.PlannedBackend {
	return deploy.PlannedBackend{Name: name, Impl: "fake",
		New: func() (func(sandbox.Confine) (sandbox.Runner, error), error) {
			return func(sandbox.Confine) (sandbox.Runner, error) {
				if b.buildErr != nil {
					return nil, b.buildErr
				}
				return b, nil
			}, nil
		}}
}

// sessionDriver is one session's driver: these tests never run a turn.
type sessionDriver struct {
	log  *journal
	name string
}

func (d *sessionDriver) Run(context.Context, agent.Request) iter.Seq2[agent.Event, error] {
	return func(func(agent.Event, error) bool) {}
}

func (d *sessionDriver) Close() error {
	d.log.add("close:driver:" + d.name)
	return nil
}

// sessionProgram is a deployment's program: it opens one driver per session and counts them.
type sessionProgram struct {
	log *journal
	// opens is atomic because the concurrency test opens several sessions at once.
	opens    atomic.Int32
	failOpen bool
}

func (p *sessionProgram) Open(agent.SessionInfo) (agent.Driver, error) {
	if p.failOpen {
		return nil, errors.New("open failed")
	}
	n := p.opens.Add(1)
	return &sessionDriver{log: p.log, name: string(rune('0' + n))}, nil
}

func (p *sessionProgram) Close() error {
	p.log.add("close:program")
	return nil
}

// defaultLimits is the session table a test deployment declares unless the test is
// about the limit itself.
func defaultLimits() policy.Limits {
	return policy.Limits{Idle: policy.Duration(time.Minute)}
}

// sessionDeployment is a deployment with one backend and a program that opens drivers.
func sessionDeployment(t *testing.T, name string, grant policy.Grant, log *journal, backend *boundary) (deploy.Deployment, *sessionProgram) {
	t.Helper()
	return sessionDeploymentWith(t, name, grant, defaultLimits(), log, backend)
}

// sessionDeploymentWith is sessionDeployment with an explicit session table.
func sessionDeploymentWith(t *testing.T, name string, grant policy.Grant, limits policy.Limits, log *journal, backend *boundary) (deploy.Deployment, *sessionProgram) {
	t.Helper()
	program := &sessionProgram{log: log}
	return deploy.Deployment{
		Name:    name,
		Limits:  limits,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return program, nil }},
	}, program
}

// permissiveGrant allows everything and names the backend the deployment configured. Files are
// unrestricted, so no directory has to exist for most tests.
func permissiveGrant(sandboxName string) policy.Grant {
	return policy.Grant{Sandbox: sandboxName}
}

// hostWith builds an Agents over a resource whose single backend is b.
func hostWith(t *testing.T, backend *boundary, opts ...AgentsOption) *Agents {
	t.Helper()
	res, err := Open(t.Context(), deploy.ProcessPlan{
		Backends: []deploy.PlannedBackend{boundBackend("local", backend)},
	}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return NewAgents(res, opts...)
}

// TestOpenSessionBuildsAndClosesEverything is the session's lifecycle: opening builds the driver and
// the boundary, closing releases both.
func TestOpenSessionBuildsAndClosesEverything(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, program := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}

	session, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := program.opens.Load(); got != 1 {
		t.Fatalf("program opens = %d, want one driver per session", got)
	}
	if session.ID() != "s1" || session.Agent() != "referee" {
		t.Fatalf("session = %q/%q", session.ID(), session.Agent())
	}
	if got := agents.Sessions("referee"); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("sessions = %v", got)
	}

	if err := agents.CloseSession(t.Context(), "s1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if got := log.joined(); got != "close:driver:1,close:sandbox" {
		t.Fatalf("events = %q, want the driver then the boundary", got)
	}
	if got := backend.closed.Load(); got != 1 {
		t.Fatalf("backend closed %d times", got)
	}
}

// TestTheBindingCarriesTheSessionsCapabilities is what the slot names are for: the tree and the
// process starter reach a tool through the binding, and nothing else does.
func TestTheBindingCarriesTheSessionsCapabilities(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = agents.CloseSession(t.Context(), "s1") }()

	tree, ok := platform.TreeSlot.Get(session.Binding().Caps)
	if !ok || tree == nil {
		t.Fatal("the file boundary is not in the binding")
	}
	if _, ok := platform.ProcessSlot.Get(session.Binding().Caps); !ok {
		t.Fatal("the process starter is not in the binding")
	}
}

// TestOpenRefusesAConfineTheBackendCannotEnforce: a read-only grant on a backend that cannot confine
// writes is refused at open, and nothing is left built.
func TestOpenRefusesAConfineTheBackendCannotEnforce(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: false}
	agents := hostWith(t, backend)
	grant := permissiveGrant("local")
	grant.Files = policy.Files{ReadOnly: true}
	dep, program := sessionDeployment(t, "referee", grant, log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := agents.Open(t.Context(), "s1", "referee", grant)
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if got := program.opens.Load(); got != 0 {
		t.Fatalf("%d drivers opened for a session that cannot exist", got)
	}
	if got := backend.closed.Load(); got != 1 {
		t.Fatalf("the refused boundary was not closed: %d", got)
	}
}

// TestMaxSessionsIsEnforced is the grant's table decision, measured per deployment.
func TestMaxSessionsIsEnforced(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	limits := defaultLimits()
	limits.MaxSessions = 1
	dep, _ := sessionDeploymentWith(t, "referee", permissiveGrant("local"), limits, log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s2", "referee", permissiveGrant("local")); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("second Open = %v, want NotAvailable", err)
	}
}

// TestRemoveRefusesWhileSessionsAreOpen keeps a deployment from vanishing under a running
// conversation.
func TestRemoveRefusesWhileSessionsAreOpen(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := agents.Remove("referee"); !errors.Is(err, ErrSessionsActive) {
		t.Fatalf("Remove = %v, want ErrSessionsActive", err)
	}
	if !agents.Deployed("referee") {
		t.Fatal("a refused Remove uninstalled the agent")
	}
}

// TestReloadKeepsTheOldProgramAliveForOpenSessions is the reload contract: a session keeps the
// program it was opened under, and the old one is released when the last of them closes.
func TestReloadKeepsTheOldProgramAliveForOpenSessions(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}

	reloaded, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Reload(reloaded); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if strings.Contains(log.joined(), "close:program") {
		t.Fatalf("events = %q: the old program was closed under an open session", log.joined())
	}

	if err := agents.CloseSession(t.Context(), "s1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if !strings.Contains(log.joined(), "close:program") {
		t.Fatalf("events = %q: the retired program was never released", log.joined())
	}
}

// TestEvictIdleClosesTheSessionsPastTheLimit keeps the idle horizon honest: a session that has done
// nothing for longer than the grant allows is closed, and one that is recent is not.
func TestEvictIdleClosesTheSessionsPastTheLimit(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "old", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := agents.Open(t.Context(), "new", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Age one session past the horizon and leave the other fresh.
	old, _ := agents.Session("old")
	old.touchNow(time.Now().Add(-2 * time.Minute))

	ids, err := agents.EvictIdle(t.Context(), "referee", time.Now())
	if err != nil {
		t.Fatalf("EvictIdle: %v", err)
	}
	if len(ids) != 1 || ids[0] != "old" {
		t.Fatalf("evicted = %v, want [old]", ids)
	}
	if _, ok := agents.Session("new"); !ok {
		t.Fatal("the recent session was evicted")
	}
}

// TestSessionsKeepInsertionOrderThroughInterleavedLifecycles is the age index's invariant: after
// however many opens, closes and idle sweeps interleave, Sessions returns exactly the live sessions
// in insertion order. The expectation is a slice the test maintains itself, so a tombstone that is
// skipped, a slot that is reused, or a compaction that reorders the survivors fails here.
func TestSessionsKeepInsertionOrderThroughInterleavedLifecycles(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rng := rand.New(rand.NewSource(7))
	now := time.Now()
	old := now.Add(-2 * time.Minute)
	var want []string
	next := 0
	for step := range 600 {
		switch rng.Intn(3) {
		case 0: // Open
			id := fmt.Sprintf("s%d", next)
			next++
			if _, err := agents.Open(t.Context(), id, "referee", permissiveGrant("local")); err != nil {
				t.Fatalf("step %d: Open(%s): %v", step, id, err)
			}
			want = append(want, id)
		case 1: // CloseSession
			if len(want) == 0 {
				continue
			}
			i := rng.Intn(len(want))
			if err := agents.CloseSession(t.Context(), want[i]); err != nil {
				t.Fatalf("step %d: CloseSession(%s): %v", step, want[i], err)
			}
			want = slices.Delete(want, i, i+1)
		case 2: // EvictIdle over a scattered subset past the horizon
			aged := make(map[string]bool, len(want)/4)
			for i, id := range want {
				if i%3 != 0 || rng.Intn(2) != 0 {
					continue
				}
				session, open := agents.Session(id)
				if !open {
					t.Fatalf("step %d: session %s is expected to be live but is not", step, id)
				}
				session.touchNow(old)
				aged[id] = true
			}
			evicted, err := agents.EvictIdle(t.Context(), "referee", now)
			if err != nil {
				t.Fatalf("step %d: EvictIdle: %v", step, err)
			}
			if len(evicted) != len(aged) {
				t.Fatalf("step %d: evicted %v, want the %d sessions past the horizon", step, evicted, len(aged))
			}
			for _, id := range evicted {
				if !aged[id] {
					t.Fatalf("step %d: evicted %s, which was not past the horizon", step, id)
				}
			}
			want = slices.DeleteFunc(want, func(id string) bool { return aged[id] })
		}
		if got := agents.Sessions("referee"); !slices.Equal(got, want) {
			t.Fatalf("step %d: Sessions = %v, want %v", step, got, want)
		}
	}
	// The index must name every live session's own slot: a stale entry would make a later close
	// tombstone somebody else, which the ordering check cannot see.
	if len(agents.slots) != len(want) {
		t.Fatalf("index holds %d ids, want the %d live sessions", len(agents.slots), len(want))
	}
	for slot, session := range agents.byAge {
		if session == nil {
			continue
		}
		if got := agents.slots[session.id]; got != slot {
			t.Fatalf("index has %s at slot %d, but it sits at %d", session.id, got, slot)
		}
	}
}

// TestOpenWithAFileGrantOpensTheTree is the bridge from a grant to real handles: the roots exist on
// this host, so the tree opens and a tool could read through it.
func TestOpenWithAFileGrantOpensTheTree(t *testing.T) {
	root := t.TempDir()
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	grant := permissiveGrant("local")
	grant.Files = policy.Files{Read: []string{root}, Write: []string{root}}
	dep, _ := sessionDeployment(t, "referee", grant, log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	session, err := agents.Open(t.Context(), "s1", "referee", grant)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = agents.CloseSession(t.Context(), "s1") }()

	tree, ok := platform.TreeSlot.Get(session.Binding().Caps)
	if !ok {
		t.Fatal("no file boundary in the binding")
	}
	target := filepath.Join(root, "note.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := tree.Read(t.Context(), target); err != nil {
		t.Fatalf("read through the session's tree: %v", err)
	}
}

// TestOpenOnAnUnknownAgentIsNotFound and the closed-table case are the two refusals a caller can hit
// before any work happens.
func TestOpenRefusals(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	if _, err := agents.Open(t.Context(), "s1", "missing", permissiveGrant("local")); !errdefs.IsNotFound(err) {
		t.Fatalf("unknown agent = %v, want NotFound", err)
	}
	if err := agents.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "missing", permissiveGrant("local")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("closed table = %v, want NotAvailable", err)
	}
}

// turnDriver is a driver that can be steered and cancelled, which is what the turn tests exercise.
type turnDriver struct {
	log       *journal
	events    []agent.Event
	steers    []agent.SteerInput
	cancelled []string
	block     chan struct{}
}

func (d *turnDriver) Run(ctx context.Context, _ agent.Request) iter.Seq2[agent.Event, error] {
	return func(yield func(agent.Event, error) bool) {
		for _, event := range d.events {
			if !yield(event, nil) {
				return
			}
		}
		if d.block != nil {
			// Wait until the test cancels, so Cancel has a turn to stop.
			<-ctx.Done()
			yield(nil, ctx.Err())
		}
	}
}

func (d *turnDriver) Close() error { return nil }

func (d *turnDriver) Steer(in agent.SteerInput) (agent.SteerID, error) {
	d.steers = append(d.steers, in)
	return agent.SteerID("steer-1"), nil
}

func (d *turnDriver) Unsteer(agent.SteerID) error { return nil }

func (d *turnDriver) Cancel(_ context.Context, reason string) error {
	d.cancelled = append(d.cancelled, reason)
	return nil
}

// turnProgram hands back one driver the test controls.
type turnProgram struct{ driver *turnDriver }

func (p *turnProgram) Open(agent.SessionInfo) (agent.Driver, error) { return p.driver, nil }
func (p *turnProgram) Close() error                                 { return nil }

// runnableHost opens one session whose driver the test holds.
func runnableHost(t *testing.T, driver *turnDriver) (*Agents, *turnProgram) {
	t.Helper()
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	program := &turnProgram{driver: driver}
	dep := deploy.Deployment{
		Name:    "referee",
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return program, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })
	return agents, program
}

// TestRunStreamsTheDriversEvents is the turn's contract: the host assembles the request, the decorator
// runs the driver, and every event reaches the caller.
func TestRunStreamsTheDriversEvents(t *testing.T) {
	driver := &turnDriver{log: &journal{}, events: []agent.Event{
		agent.Text{Text: "hello"},
		agent.Status{State: agent.StateCompleted},
	}}
	agents, _ := runnableHost(t, driver)

	var seen []agent.EventKind
	for event, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		seen = append(seen, event.Kind())
	}
	if len(seen) != 2 || seen[0] != agent.KindText || seen[1] != agent.KindStatus {
		t.Fatalf("events = %v", seen)
	}
	if session, _ := agents.Session("s1"); session.ordinal != 1 {
		t.Fatalf("ordinal = %d, want one turn counted", session.ordinal)
	}
}

// TestASecondRunIsRefused: one session has at most one turn in flight, which is the rule Steer and
// Cancel rely on.
func TestASecondRunIsRefused(t *testing.T) {
	driver := &turnDriver{log: &journal{}, block: make(chan struct{})}
	agents, _ := runnableHost(t, driver)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(ctx, "s1", message.NewTextMessage(message.RoleUser, "first")) {
		}
	}()
	// Wait until the first turn is streaming.
	waitRunning(t, agents, "s1")

	var failure error
	for _, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "second")) {
		failure = err
	}
	if failure == nil || !errdefs.IsConflict(failure) {
		t.Fatalf("second Run = %v, want a conflict", failure)
	}
	cancel()
	<-done
}

// TestSteerReachesTheDriver keeps the routing honest: the host does not interpret the input, it hands
// it to the session's driver.
func TestSteerReachesTheDriver(t *testing.T) {
	driver := &turnDriver{log: &journal{}, block: make(chan struct{})}
	agents, _ := runnableHost(t, driver)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(ctx, "s1", message.NewTextMessage(message.RoleUser, "first")) {
		}
	}()
	waitRunning(t, agents, "s1")

	id, err := agents.Steer(t.Context(), "s1", agent.SteerInput{Intent: agent.SteerRedirect})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if id != "steer-1" {
		t.Fatalf("id = %q", id)
	}
	if len(driver.steers) != 1 || driver.steers[0].Intent != agent.SteerRedirect {
		t.Fatalf("steers = %+v", driver.steers)
	}
	cancel()
	<-done
}

// TestSteerOnAnIdleSessionSaysSo: nothing is in flight, so there is nothing to steer.
func TestSteerOnAnIdleSessionSaysSo(t *testing.T) {
	driver := &turnDriver{log: &journal{}}
	agents, _ := runnableHost(t, driver)
	if _, err := agents.Steer(t.Context(), "s1", agent.SteerInput{}); !errors.Is(err, agent.ErrSessionIdle) {
		t.Fatalf("Steer = %v, want ErrSessionIdle", err)
	}
}

// TestCancelStopsTheTurnAndTellsThePeer covers both halves of cancellation: the context ends the
// local steps, and a driver that talks to a peer is told as well.
func TestCancelStopsTheTurnAndTellsThePeer(t *testing.T) {
	driver := &turnDriver{log: &journal{}, block: make(chan struct{})}
	agents, _ := runnableHost(t, driver)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "first")) {
		}
	}()
	waitRunning(t, agents, "s1")

	if err := agents.Cancel(t.Context(), "s1", "operator"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	<-done
	if len(driver.cancelled) != 1 || driver.cancelled[0] != "operator" {
		t.Fatalf("cancelled = %v", driver.cancelled)
	}
	// The turn ended, so a second Cancel has nothing to stop.
	if err := agents.Cancel(t.Context(), "s1", "again"); !errors.Is(err, agent.ErrSessionIdle) {
		t.Fatalf("second Cancel = %v, want ErrSessionIdle", err)
	}
}

// waitRunning blocks until the session reports a turn in flight, so the tests above do not race the
// goroutine they just started.
func waitRunning(t *testing.T, agents *Agents, sessionID string) {
	t.Helper()
	for range 1000 {
		if session, ok := agents.Session(sessionID); ok && session.isRunning() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the turn never started")
}

// TestTheSessionLimitHoldsUnderConcurrentOpens is the check the reviewer found missing: the limit was
// checked before the build, and the build happens outside the lock.
func TestTheSessionLimitHoldsUnderConcurrentOpens(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	limits := defaultLimits()
	limits.MaxSessions = 1
	dep, _ := sessionDeploymentWith(t, "referee", permissiveGrant("local"), limits, log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(ordinal int) {
			defer wg.Done()
			_, err := agents.Open(context.Background(), fmt.Sprintf("s%d", ordinal), "referee", permissiveGrant("local"))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	opened := 0
	for err := range results {
		if err == nil {
			opened++
		}
	}
	if opened != 1 {
		t.Fatalf("opened = %d sessions, want exactly the limit", opened)
	}
	if got := len(agents.Sessions("referee")); got != 1 {
		t.Fatalf("table holds %d sessions, want 1", got)
	}
}

// TestCloseReleasesARetiredProgram is the leak the reviewer found: a reload retires a deployment, and the
// table must still close its program when it shuts down.
func TestCloseReleasesARetiredProgram(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	reloaded, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Reload(reloaded); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if strings.Contains(log.joined(), "close:program") {
		t.Fatalf("events = %q: the retired program closed while its session was open", log.joined())
	}

	if err := agents.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := strings.Count(log.joined(), "close:program"); got != 2 {
		t.Fatalf("program closes = %d (%s), want both the live and the retired one", got, log.joined())
	}
}

// TestClosingASessionStopsItsRunningTurn is the coordination a close owes a turn: stop it, then
// wait for it, then release the driver and the boundary. Closing underneath a streaming turn let a
// command run on a closed runner and raced on the driver field.
func TestClosingASessionStopsItsRunningTurn(t *testing.T) {
	driver := &turnDriver{log: &journal{}, block: make(chan struct{})}
	agents, _ := runnableHost(t, driver)

	done := make(chan error, 1)
	go func() {
		var failure error
		for _, err := range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
			if err != nil {
				failure = err
			}
		}
		done <- failure
	}()
	waitRunning(t, agents, "s1")

	closed := make(chan error, 1)
	go func() { closed <- agents.CloseSession(t.Context(), "s1") }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseSession: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseSession waited forever on the running turn")
	}
	select {
	case failure := <-done:
		// The turn was stopped, not failed: cancellation reaches the driver as an error, and
		// what matters here is that it ended before the close returned.
		if failure != nil && !errors.Is(failure, context.Canceled) {
			t.Fatalf("turn = %v, want a cancellation", failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the turn outlived the close")
	}
	close(driver.block)
}

// TestEvictIdleSkipsARunningTurn is the idle limit's blind spot: a turn's last touch is the
// previous turn's, so a model call longer than the limit made its own session look abandoned and
// evicted it mid-run.
func TestEvictIdleSkipsARunningTurn(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	limits := defaultLimits()
	limits.Idle = policy.Duration(time.Millisecond)
	blocked := &turnDriver{log: log, block: make(chan struct{})}
	dep := deploy.Deployment{
		Name:    "referee",
		Limits:  limits,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) { return &turnProgram{driver: blocked}, nil }},
	}
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() {
		close(blocked.block)
		_ = agents.Close(t.Context())
	})
	if _, err := agents.Open(t.Context(), "s1", "referee", permissiveGrant("local")); err != nil {
		t.Fatalf("Open: %v", err)
	}

	go func() {
		for range agents.Run(t.Context(), "s1", message.NewTextMessage(message.RoleUser, "hi")) {
		}
	}()
	waitRunning(t, agents, "s1")

	evicted, err := agents.EvictIdle(t.Context(), "referee", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("EvictIdle: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("evicted %v, want a running turn to be left alone", evicted)
	}
	if _, open := agents.Session("s1"); !open {
		t.Fatal("the running session was evicted")
	}
}

// TestASessionRunsUnderTheGrantItWasOpenedWith is the authority rule: the caller states it, the
// binding carries it, and the digest that goes on every span names exactly that value.
func TestASessionRunsUnderTheGrantItWasOpenedWith(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)

	authority := permissiveGrant("local")
	authority.Tools = policy.ToolSet{Allow: []string{"read_file"}}
	dep, _ := sessionDeployment(t, "referee", authority, log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	session, err := agents.Open(t.Context(), "s1", "referee", authority)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	bind := session.Binding().Grant
	if bind.AllowsTool("write_file") || bind.AllowsTool("exec") {
		t.Fatalf("bind allows %v, want only the stated tool", bind.Tools.Allow)
	}
	if !bind.AllowsTool("read_file") {
		t.Fatal("bind lost the tool the caller stated")
	}
	if session.bindID == "" || session.bindID != bind.Digest() {
		t.Fatalf("bind id = %q, want the grant's digest %q", session.bindID, bind.Digest())
	}
}

// TestAnOpenWithAnUnenforceableGrantIsRefused: the grant is validated where it enters, and a
// refused open leaves no session behind.
func TestAnOpenWithAnUnenforceableGrantIsRefused(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	// A narrowed read set beside an unrestricted write set is the combination policy refuses.
	refused := policy.Grant{Files: policy.Files{Read: []string{pathtest.Abs("srv/w")}}}
	if _, err := agents.Open(t.Context(), "s1", "referee", refused); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if _, open := agents.Session("s1"); open {
		t.Fatal("a refused open left a session behind")
	}
}

// TestAnOpenWithAGrantTheProcessCannotCarryIsRefused is the other side of the same check: the
// grant is fine, this process does not have the backend it names.
func TestAnOpenWithAGrantTheProcessCannotCarryIsRefused(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	if _, err := agents.Open(t.Context(), "s1", "referee", policy.Grant{Sandbox: "bwrap"}); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
}

// TestCompactionKeepsTheIndexAndTheOrder is the part of the age index that a random interleaving
// will not reliably reach: compaction. Tombstoned slots are collapsed once they outnumber the live
// ones, and that re-indexing is the only place the position map can drift from the slice. The test
// forces the condition instead of hoping for it, then checks both that `Sessions` still reports
// insertion order and that every live session is still addressable — a stale index would show up
// as a close that reports "not open" for a session that is.
func TestCompactionKeepsTheIndexAndTheOrder(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	// Open a batch, close every other one — more tombstones than survivors, so the next close
	// triggers a compaction.
	const batch = 40
	var live []string
	for i := range batch {
		id := fmt.Sprintf("s%02d", i)
		if _, err := agents.Open(t.Context(), id, "referee", permissiveGrant("local")); err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
		if i%2 == 0 {
			live = append(live, id)
			continue
		}
		if err := agents.CloseSession(t.Context(), id); err != nil {
			t.Fatalf("CloseSession(%s): %v", id, err)
		}
	}

	// Order survives: the survivors are still in insertion order.
	if got := agents.Sessions("referee"); !slices.Equal(got, live) {
		t.Fatalf("Sessions = %v, want %v", got, live)
	}
	// And every survivor is still addressable through the index: closing one must find it.
	first := live[0]
	if err := agents.CloseSession(t.Context(), first); err != nil {
		t.Fatalf("CloseSession(%s) after compaction: %v", first, err)
	}
	if _, open := agents.Session(first); open {
		t.Fatal("a closed session is still in the table")
	}
	if got := agents.Sessions("referee"); len(got) != len(live)-1 {
		t.Fatalf("Sessions = %v, want %d entries", got, len(live)-1)
	}
}

// TestAnUnstatedSandboxNeedsOneBackend covers the rule that makes an empty grant usable: an empty
// sandbox name asks for the runtime's default, and a process with none or several has none to give.
func TestAnUnstatedSandboxNeedsOneBackend(t *testing.T) {
	log := &journal{}
	backend := &boundary{log: log, filesystemBounds: true}
	agents := hostWith(t, backend)
	dep, _ := sessionDeployment(t, "referee", permissiveGrant("local"), log, backend)
	if err := agents.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = agents.Close(t.Context()) })

	// One backend, no name: it is the default.
	if _, err := agents.Open(t.Context(), "s1", "referee", policy.Grant{}); err != nil {
		t.Fatalf("Open with one backend and no name: %v", err)
	}

	// Two backends, no name: the process cannot guess, and the error says what it runs.
	two, err := Open(t.Context(), deploy.ProcessPlan{Backends: []deploy.PlannedBackend{
		boundBackend("local", backend),
		boundBackend("bwrap", backend),
	}}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = two.Close() })
	ambiguous := NewAgents(two)
	if err := ambiguous.Add(dep); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { _ = ambiguous.Close(t.Context()) })
	_, err = ambiguous.Open(t.Context(), "s1", "referee", policy.Grant{})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
	if !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("error %q does not say which backends this process runs", err)
	}
}
