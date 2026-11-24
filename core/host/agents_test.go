package host

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// program is a deployment's compiled program: these tests never run a turn, they only assert the
// table's lifecycle, so Open is not exercised.
type program struct {
	name     string
	journal  *journal
	closeErr error
}

func (p *program) Open(agent.SessionInfo) (agent.Driver, error) { return nil, errors.New("not used") }

func (p *program) Close() error {
	p.journal.add("close:" + p.name)
	return p.closeErr
}

// plannedProgram is a deployment whose program records its own closure.
func plannedProgram(name string, log *journal, closeErr error) deploy.Deployment {
	return deploy.Deployment{
		Name: name,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) {
			return &program{name: name, journal: log, closeErr: closeErr}, nil
		}},
	}
}

// failingProgram is a deployment whose program cannot be built.
func failingProgram(name string) deploy.Deployment {
	return deploy.Deployment{
		Name: name,
		Program: deploy.PlannedProgram{Impl: "graph", New: func() (agent.Program, error) {
			return nil, errors.New("compile failed")
		}},
	}
}

func newAgents(t *testing.T) *Agents {
	t.Helper()
	res, err := Open(t.Context(), deploy.ProcessPlan{}, testOptions(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return NewAgents(res)
}

// TestAddInstallsAndCloseUninstallsNewestFirst pins the table's lifecycle: a deployment owns its
// program, and closing the table closes them in the reverse of installation.
func TestAddInstallsAndCloseUninstallsNewestFirst(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	if err := agents.Add(plannedProgram("referee", log, nil), plannedProgram("seat_1", log, nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := strings.Join(agents.Names(), ","); got != "referee,seat_1" {
		t.Fatalf("names = %q", got)
	}
	if err := agents.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := log.joined(); got != "close:seat_1,close:referee" {
		t.Fatalf("events = %q, want newest first", got)
	}
	if len(agents.Names()) != 0 {
		t.Fatal("the table kept its entries after Close")
	}
}

// TestAddIsAllOrNothing is why the build happens before the insert: half an installed set is a
// process whose behaviour does not match its name.
func TestAddIsAllOrNothing(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	err := agents.Add(plannedProgram("referee", log, nil), failingProgram("seat_1"))
	if err == nil {
		t.Fatal("Add accepted a deployment whose program failed")
	}
	if !strings.Contains(err.Error(), "seat_1") {
		t.Fatalf("error %q does not name the failing agent", err)
	}
	if got := log.joined(); got != "close:referee" {
		t.Fatalf("events = %q, want the built program closed again", got)
	}
	if len(agents.Names()) != 0 {
		t.Fatalf("the table kept %v", agents.Names())
	}
}

func TestAddRefusesADuplicateName(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	if err := agents.Add(plannedProgram("referee", log, nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := agents.Add(plannedProgram("referee", log, nil))
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	// The program built for the refused deployment is closed; the installed one is not.
	if got := log.joined(); got != "close:referee" {
		t.Fatalf("events = %q", got)
	}
	if !agents.Deployed("referee") {
		t.Fatal("the installed deployment was removed")
	}
}

func TestRemoveClosesTheProgram(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	if err := agents.Add(plannedProgram("referee", log, nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := agents.Remove("referee"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := log.joined(); got != "close:referee" {
		t.Fatalf("events = %q", got)
	}
	if agents.Deployed("referee") {
		t.Fatal("the deployment is still installed")
	}
	if err := agents.Remove("referee"); !errdefs.IsNotFound(err) {
		t.Fatalf("second Remove = %v, want not found", err)
	}
}

// TestReloadSwapsTheProgramAndClosesTheOld keeps the table's promise: the name stays, the program
// is replaced, and the one it replaced is released.
func TestReloadSwapsTheProgramAndClosesTheOld(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	if err := agents.Add(plannedProgram("referee", log, nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := agents.Reload(plannedProgram("referee", log, nil)); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := log.joined(); got != "close:referee" {
		t.Fatalf("events = %q, want the previous program closed", got)
	}
	if names := agents.Names(); len(names) != 1 {
		t.Fatalf("names = %v, want the agent still installed once", names)
	}
}

func TestReloadRequiresAnInstalledAgent(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	err := agents.Reload(plannedProgram("nobody", log, nil))
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
	if got := log.joined(); got != "close:nobody" {
		t.Fatalf("events = %q, want the built program closed", got)
	}
}

// TestCloseJoinsFailuresNotStopsAtThem: one program refusing to close is not a reason to leak the
// rest.
func TestCloseJoinsFailures(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	if err := agents.Add(
		plannedProgram("referee", log, nil),
		plannedProgram("seat_1", log, errors.New("close failed")),
	); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := agents.Close(t.Context())
	if err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("err = %v, want the close failure", err)
	}
	if got := log.joined(); got != "close:seat_1,close:referee" {
		t.Fatalf("events = %q, want every program closed", got)
	}
}

// TestAClosedTableAcceptsNothing keeps a closed table closed: a program installed after Close would
// never be closed.
func TestAClosedTableAcceptsNothing(t *testing.T) {
	log := &journal{}
	agents := newAgents(t)
	if err := agents.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := agents.Add(plannedProgram("late", log, nil)); err == nil {
		t.Fatal("a closed table accepted a deployment")
	}
	if got := log.joined(); got != "close:late" {
		t.Fatalf("events = %q, want the refused program closed", got)
	}
}
