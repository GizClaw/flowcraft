// Package sandboxtest is the runner conformance suite: the same assertions
// every core/sandbox backend runs against itself.
//
// It exists because the contract is prose and prose drifts. A backend is
// verified by driving a real runner — real processes, real output — and
// checking the promises core/sandbox makes: capabilities that match behaviour,
// a read cursor over an append-only log, EOF as a value and not a close, the
// /dev/null stdin rule, an idempotent lifecycle, and a declaration that rejects
// the policy it cannot enforce.
//
// A one-shot Exec, a new transport or a new platform does not need a new
// reading of the contract — it needs this suite to pass. The remote backend's
// acceptance test is exactly that: wrap a local runner in an HTTP hop and run
// the same suite through the wire.
//
// # Using it
//
//	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
//		runner := local.New(root)
//		t.Cleanup(func() { _ = runner.Close() })
//		return sandboxtest.Harness{Runner: runner, Root: root}
//	})
//
// The builder owns the runner and registers its cleanup; the suite owns the
// root directory (it passes t.TempDir()) and never closes anything itself.
//
// # What it does not cover
//
// The file journal has its own suite (journal/journaltest), because a journal
// is opt-in per runner. ErrSequenceGap is not forced here either: the replay
// window is a construction-time parameter, not part of the Runner interface, so
// only the backend that owns the window can provoke a gap.
package sandboxtest

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// Timeouts. They are deliberately generous: the suite runs on shared CI hosts
// and its job is to catch a semantic break, not to benchmark anything.
const (
	// stepTimeout bounds one ordinary command or one blocking Read.
	stepTimeout = 15 * time.Second
	// probeTimeout bounds a command that must return on its own. It is what
	// distinguishes "the child read EOF" from "the child is blocked on a pipe".
	probeTimeout = 3 * time.Second
	// settleTimeout bounds cleanup waits.
	settleTimeout = 10 * time.Second
)

// Harness is one backend, ready to be driven.
type Harness struct {
	// Runner is the backend under test.
	Runner sandbox.Runner
	// Root is the directory commands run in. The suite never creates it: the
	// builder receives it and roots the runner there.
	Root string
	// Shell renders scripts for the platform the backend runs on. Nil means
	// ForPlatform(runtime.GOOS).
	Shell Shell
	// OwnsResources marks a runner that replaces ExecOptions.Resources instead
	// of passing it through. engine.WithDefaults does exactly that by design
	// ("defaults wins entirely"), so the two subtests that assert a caller's
	// cap reaches the backend are skipped rather than failed. A backend leaves
	// this false.
	OwnsResources bool
}

// Builder constructs one backend rooted at root. It is called once per
// subtest, so each assertion starts from a clean runner.
type Builder func(t *testing.T, root string) Harness

// Run exercises the contract. Backends call it from their own test file so a
// failure names the backend that broke.
//
// Each subtest builds its own runner from build, so one assertion breaking does
// not cascade into the next, and the list below is the contract's table of
// contents: adding a promise means adding a row and a function.
func Run(t *testing.T, build Builder) {
	t.Helper()

	for _, subtest := range []struct {
		name string
		run  func(*testing.T, Builder)
	}{
		{"capabilities match behaviour", testCapabilitiesMatchBehaviour},
		{"exec runs one command", testExecRunsOneCommand},
		{"a non-zero exit is a result", testANonZeroExitIsAResult},
		{"exec honours the timeout", testExecHonoursTheTimeout},
		{"exec truncates per stream", testExecTruncatesPerStream},
		{"read is a cursor over an append-only log", testReadIsACursorOverAnAppendOnlyLog},
		{"input surface matches the spec", testInputSurfaceMatchesTheSpec},
		{"terminate is idempotent and visible in list", testTerminateIsIdempotentAndVisibleInList},
		{"list is stable and ordered", testListIsStableAndOrdered},
		{"no advertised sessions means no exec", testNoAdvertisedSessionsMeansNoExec},
		{"the policy gate agrees with the declaration", testThePolicyGateAgreesWithTheDeclaration},
		{"close terminates every session and is idempotent", testCloseTerminatesEverySessionAndIsIdempotent},
	} {
		t.Run(subtest.name, func(t *testing.T) { subtest.run(t, build) })
	}
}

func testCapabilitiesMatchBehaviour(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	caps := h.Runner.Capabilities()
	sessions := caps.Features

	if !sessions.TTY {
		requireNotAvailable(t, startSession(t, h, sandbox.SessionSpec{
			Argv: h.shell().Argv("echo tty"),
			TTY:  true,
		}), "Start(TTY) on a runner that does not advertise TTY")
	} else {
		sess := startOK(t, h, sandbox.SessionSpec{
			Argv: h.shell().Argv("echo tty"),
			TTY:  true,
			Rows: 24,
			Cols: 80,
		})
		if !sess.Capabilities().TTY {
			t.Error("session does not report the TTY the runner advertised")
		}
		if err := sess.Resize(context.Background(), 40, 120); err != nil {
			t.Errorf("Resize on an advertised TTY: %v", err)
		}
		_ = sess.Close()
	}

	sess := startOK(t, h, sandbox.SessionSpec{Argv: h.shell().Argv("echo signal")})
	if !sessions.Signal {
		requireNotAvailable(t, sess.Signal(context.Background(), sandbox.SessionSignalInterrupt),
			"Signal on a runner that does not advertise it")
	} else if err := sess.Signal(context.Background(), sandbox.SessionSignalInterrupt); err != nil {
		t.Errorf("Signal on an advertised session: %v", err)
	}
	if !sessions.Events {
		watcher, werr := sess.Watch(context.Background())
		if werr == nil {
			_ = watcher.Close()
		}
		requireNotAvailable(t, werr,
			"Watch on a runner that does not advertise events")
	}
	_ = sess.Close()
}

func testExecRunsOneCommand(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	result, err := h.exec(t, "echo hello-contract")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (stderr: %s)", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello-contract") {
		t.Fatalf("Stdout = %q, want it to contain %q", result.Stdout, "hello-contract")
	}
}

// A failing command is a result, not an error: callers branch on ExitCode.
func testANonZeroExitIsAResult(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	result, err := h.exec(t, "exit 3")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", result.ExitCode)
	}
}

// "Implementations MUST honour ExecOptions.Timeout" — so a killed command
// must read as a timeout rather than as a plain failure.
func testExecHonoursTheTimeout(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	_, err := engine.Exec(context.Background(), h.Runner,
		h.shell().Argv(h.shell().LongRunning())[0],
		h.shell().Argv(h.shell().LongRunning())[1:],
		sandbox.ExecOptions{WorkDir: h.Root, Timeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("Exec of a long-running command returned no error")
	}
	if !errdefs.IsTimeout(err) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout classification", err)
	}
}

func testExecTruncatesPerStream(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	if h.OwnsResources {
		t.Skip("this runner owns ExecOptions.Resources; a caller cap never reaches the backend")
	}
	const cap = 16
	result, err := engine.Exec(context.Background(), h.Runner,
		h.shell().Argv("echo 0123456789012345678901234567890123456789")[0],
		h.shell().Argv("echo 0123456789012345678901234567890123456789")[1:],
		sandbox.ExecOptions{
			WorkDir:   h.Root,
			Resources: sandbox.ResourceLimits{MaxOutputBytes: cap},
		})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(result.Stdout) > cap {
		t.Fatalf("Stdout is %d bytes, want at most %d", len(result.Stdout), cap)
	}
}

func testReadIsACursorOverAnAppendOnlyLog(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	sess := startOK(t, h, sandbox.SessionSpec{Argv: h.shell().Argv("echo abcdefgh")})

	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()

	var (
		got      []byte
		afterSeq int64
		lastSeq  int64 = -1
	)
	for {
		out, err := sess.Read(ctx, afterSeq, 4)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, chunk := range out.Chunks {
			if chunk.Seq < lastSeq {
				t.Fatalf("chunk seq %d went backwards from %d", chunk.Seq, lastSeq)
			}
			lastSeq = chunk.Seq
			got = append(got, chunk.Data...)
		}
		if out.NextSeq < afterSeq {
			t.Fatalf("NextSeq %d went backwards from %d", out.NextSeq, afterSeq)
		}
		afterSeq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if !strings.Contains(string(got), "abcdefgh") {
		t.Fatalf("output = %q, want it to contain %q", got, "abcdefgh")
	}

	// EOF is a value, not a close: the log stays readable, and re-reading
	// from an earlier cursor replays.
	out, err := sess.Read(ctx, afterSeq, 4096)
	if err != nil {
		t.Fatalf("Read after EOF: %v", err)
	}
	if !out.EOF || len(out.Chunks) != 0 {
		t.Fatalf("Read after EOF = %+v, want EOF with no chunks", out)
	}
	if afterSeq == 0 {
		// Nothing was produced; there is no earlier position to replay.
		_ = sess.Close()
		return
	}
	replay, err := sess.Read(ctx, 0, 4)
	if err != nil {
		t.Fatalf("replay from 0: %v", err)
	}
	if len(replay.Chunks) == 0 || replay.Chunks[0].Seq != 0 {
		t.Fatalf("replay = %+v, want the log to start at seq 0", replay)
	}
	_ = sess.Close()
}

// The stdin rule: fd 0 is /dev/null unless the spec declares an input
// side. A program that branches on stdin must see EOF, not a pipe nobody
// will ever close.
func testInputSurfaceMatchesTheSpec(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	shell := h.shell()
	runnerArgv := shell.StdinReader()

	sess := startOK(t, h, sandbox.SessionSpec{Argv: runnerArgv})
	if sess.Capabilities().Stdin {
		t.Error("session reports an input side the spec never declared")
	}
	if err := sess.Write(context.Background(), []byte("ignored")); !errdefs.IsNotAvailable(err) {
		t.Errorf("Write on an undeclared input = %v, want NotAvailable", err)
	}
	// Closing an undefined input is a no-op success, so a caller that
	// always closes needs no special case.
	if err := sess.CloseInput(); err != nil {
		t.Errorf("CloseInput on an undeclared input: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var out []byte
	afterSeq := int64(0)
	for {
		chunk, err := sess.Read(ctx, afterSeq, 4096)
		if err != nil {
			t.Fatalf("reading %v with no stdin declared: %v (a child blocked on fd 0 means the spec did not get /dev/null)", runnerArgv, err)
		}
		for _, c := range chunk.Chunks {
			out = append(out, c.Data...)
		}
		afterSeq = chunk.NextSeq
		if chunk.EOF {
			break
		}
	}
	if len(out) != 0 {
		t.Errorf("output = %q, want none (stdin was /dev/null, so nothing was echoed)", out)
	}
	_ = sess.Close()

	// The declared form: a real pipe, written bytes arrive.
	sess = startOK(t, h, sandbox.SessionSpec{Argv: runnerArgv, Stdin: true})
	if !sess.Capabilities().Stdin {
		t.Fatal("session does not report the input side the spec declared")
	}
	if err := sess.Write(context.Background(), []byte("through-the-pipe")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sess.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}
	out = nil
	afterSeq = 0
	ctx2, cancel2 := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel2()
	for {
		chunk, err := sess.Read(ctx2, afterSeq, 4096)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, c := range chunk.Chunks {
			out = append(out, c.Data...)
		}
		afterSeq = chunk.NextSeq
		if chunk.EOF {
			break
		}
	}
	if !strings.Contains(string(out), "through-the-pipe") {
		t.Errorf("output = %q, want the written bytes", out)
	}
	_ = sess.Close()
}

// Two handles terminate the same process and their guarantees differ: the
// manager's Terminate(id) syncs the record, so List reflects the kill as
// soon as it returns; the session handle's Terminate owns the process, and
// List catches up through the exit it produces. The suite pins both.
func testTerminateIsIdempotentAndVisibleInList(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	sess := startOK(t, h, sandbox.SessionSpec{
		ID:   "terminate-contract",
		Argv: h.shell().Argv(h.shell().LongRunning()),
	})
	if !containsRunning(listOK(t, h), sess.ID()) {
		t.Fatalf("List does not report the started session %q", sess.ID())
	}
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	if err := h.Runner.Terminate(ctx, sess.ID()); err != nil {
		t.Fatalf("Runner.Terminate: %v", err)
	}
	if running := containsRunning(listOK(t, h), sess.ID()); running {
		t.Fatalf("List still reports %q as running right after Runner.Terminate", sess.ID())
	}
	// Idempotent on an exited process, through both handles.
	if err := h.Runner.Terminate(ctx, sess.ID()); err != nil {
		t.Fatalf("second Runner.Terminate: %v", err)
	}
	if err := sess.Terminate(ctx); err != nil {
		t.Fatalf("Session.Terminate on an exited process: %v", err)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after Terminate: %v", err)
	}
	if exit.Reason != sandbox.SessionTerminated {
		t.Fatalf("exit reason = %v, want terminated", exit.Reason)
	}
	// The output log survives the kill.
	if _, err := sess.Read(ctx, 0, 4096); err != nil && !errors.Is(err, sandbox.ErrSequenceGap) {
		t.Fatalf("Read after Terminate: %v", err)
	}
	_ = sess.Close()
}

func testListIsStableAndOrdered(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	ids := make([]string, 0, 3)
	for i := range 3 {
		spec := sandbox.SessionSpec{Argv: h.shell().Argv(h.shell().LongRunning())}
		sess := startOK(t, h, spec)
		ids = append(ids, sess.ID())
		_ = i
	}
	first := listOK(t, h)
	second := listOK(t, h)
	if len(first) != len(second) {
		t.Fatalf("List is not stable: %d then %d entries", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("List order changed at %d: %q then %q", i, first[i].ID, second[i].ID)
		}
		if i > 0 && first[i].StartedAt.Before(first[i-1].StartedAt) {
			t.Fatalf("List is not ordered by start time: %v after %v",
				first[i].StartedAt, first[i-1].StartedAt)
		}
	}
	for _, id := range ids {
		if !containsRunning(first, id) {
			t.Fatalf("List does not report %q", id)
		}
	}
	terminateAll(t, h)
}

func testNoAdvertisedSessionsMeansNoExec(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	if h.Runner.Capabilities().Features != (sandbox.SessionFeatures{}) {
		t.Skip("backend serves sessions")
	}
	_, err := h.exec(t, "echo anything")
	requireNotAvailable(t, err, "Exec on a backend with no session support")
}

func testThePolicyGateAgreesWithTheDeclaration(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	if h.OwnsResources {
		t.Skip("this runner owns ExecOptions.Resources; the gate sees only its own values")
	}
	caps := h.Runner.Capabilities().Policy
	if !h.sessionsWork(t) {
		t.Skip("backend serves no sessions")
	}

	// A cap the runner does not enforce is rejected up front, never
	// silently dropped. This is the check that catches a backend which
	// forgets to pass its own Enforcement to ValidateExecPolicy.
	memory := sandbox.SessionSpec{
		Argv: h.shell().Argv("echo caps"),
		Opts: sandbox.ExecOptions{
			WorkDir:   h.Root,
			Resources: sandbox.ResourceLimits{MemoryBytes: 64 << 20},
		},
	}
	sess, err := h.Runner.Start(context.Background(), memory)
	if caps.MemoryCap {
		if err != nil {
			t.Fatalf("MemoryBytes rejected by a runner that advertises MemoryCap: %v", err)
		}
		_ = sess.Close()
	} else {
		requireNotAvailable(t, err, "MemoryBytes on a runner that does not advertise MemoryCap")
	}

	cpu := sandbox.SessionSpec{
		Argv: h.shell().Argv("echo caps"),
		Opts: sandbox.ExecOptions{
			WorkDir:   h.Root,
			Timeout:   time.Second,
			Resources: sandbox.ResourceLimits{CPUMillicores: 100},
		},
	}
	sess, err = h.Runner.Start(context.Background(), cpu)
	if caps.CPUCap {
		if err != nil {
			t.Fatalf("CPUMillicores rejected by a runner that advertises CPUCap: %v", err)
		}
		_ = sess.Close()
	} else {
		requireNotAvailable(t, err, "CPUMillicores on a runner that does not advertise CPUCap")
	}

	// Disk quotas have no mechanism anywhere, so the gate rejects them
	// regardless of what a backend advertises.
	disk := sandbox.SessionSpec{
		Argv: h.shell().Argv("echo caps"),
		Opts: sandbox.ExecOptions{
			WorkDir:   h.Root,
			Resources: sandbox.ResourceLimits{DiskBytes: 1 << 20},
		},
	}
	requireNotAvailable(t, startSession(t, h, disk), "DiskBytes (no backend enforces a quota)")
}

func testCloseTerminatesEverySessionAndIsIdempotent(t *testing.T, build Builder) {
	h := build(t, t.TempDir())
	requireSessions(t, h)
	sess := startOK(t, h, sandbox.SessionSpec{Argv: h.shell().Argv(h.shell().LongRunning())})
	if err := h.Runner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
	defer cancel()
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after Close: %v", err)
	}
	if exit.Reason == sandbox.SessionExited && exit.Code == 0 {
		t.Fatalf("session exited 0 after Close; a running child must be terminated")
	}
	if err := h.Runner.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

// Shell renders the suite's scripts and programs for one platform.
//
// Only the two genuinely platform-specific pieces are verbs: a script that runs
// until killed, and a program that reads stdin to EOF. Everything else is a
// script the caller writes ("echo x", "exit 3"), which both POSIX shells and
// cmd.exe understand.
type Shell interface {
	// Argv renders one script into the argv that runs it.
	Argv(script string) []string
	// LongRunning is a script that keeps running until it is killed. It is
	// how the suite holds a session open.
	LongRunning() string
	// StdinReader is the argv of a program that copies stdin to stdout and
	// exits 0 at EOF. It is how the suite proves whether fd 0 is a pipe: a
	// child on /dev/null exits at once, a child on a pipe nobody closes
	// blocks until the suite gives up.
	StdinReader() []string
}

// POSIX renders scripts for sh(1): the shell every unix backend already uses.
type POSIX struct{}

// Argv implements Shell.
func (POSIX) Argv(script string) []string { return []string{"sh", "-c", script} }

// LongRunning implements Shell.
func (POSIX) LongRunning() string { return "sleep 30" }

// StdinReader implements Shell.
func (POSIX) StdinReader() []string { return []string{"cat"} }

// Cmd renders scripts for cmd.exe, the shell the Windows backend runs.
type Cmd struct{}

// Argv implements Shell.
func (Cmd) Argv(script string) []string { return []string{"cmd", "/c", script} }

// LongRunning implements Shell: ping is the portable cmd.exe sleep, since
// timeout(1) needs a console and fails when stdin is not one.
func (Cmd) LongRunning() string { return "ping -n 31 127.0.0.1 >nul" }

// StdinReader implements Shell.
func (Cmd) StdinReader() []string { return []string{"cmd", "/c", "more"} }

// ForPlatform returns the shell implementations this package ships for goos.
func ForPlatform(goos string) Shell {
	if goos == "windows" {
		return Cmd{}
	}
	return POSIX{}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (h Harness) shell() Shell {
	if h.Shell != nil {
		return h.Shell
	}
	return ForPlatform(runtime.GOOS)
}

// exec runs one script to completion inside the sandbox.
func (h Harness) exec(t *testing.T, script string) (*sandbox.ExecResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	argv := h.shell().Argv(script)
	return engine.Exec(ctx, h.Runner, argv[0], argv[1:],
		sandbox.ExecOptions{WorkDir: h.Root})
}

// requireSessions skips a subtest on a backend that serves no sessions at all
// (a lifecycle-only runner). The negative assertions live in their own subtest,
// so nothing is silently skipped here.
func requireSessions(t *testing.T, h Harness) {
	t.Helper()
	if !h.sessionsWork(t) {
		t.Skip("backend serves no sessions")
	}
}

// sessionsWork reports whether the backend can start a session at all.
func (h Harness) sessionsWork(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	sess, err := h.Runner.Start(ctx, sandbox.SessionSpec{Argv: h.shell().Argv("echo probe")})
	if err != nil {
		return false
	}
	_ = sess.Close()
	return true
}

func startSession(t *testing.T, h Harness, spec sandbox.SessionSpec) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	sess, err := h.Runner.Start(ctx, spec)
	if err == nil {
		t.Cleanup(func() { _ = sess.Close() })
	}
	return err
}

func startOK(t *testing.T, h Harness, spec sandbox.SessionSpec) sandbox.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	sess, err := h.Runner.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start(%q): %v", strings.Join(spec.Argv, " "), err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func listOK(t *testing.T, h Harness) []sandbox.SessionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	list, err := h.Runner.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return list
}

func containsRunning(list []sandbox.SessionInfo, id string) bool {
	for _, info := range list {
		if info.ID == id {
			return info.Running
		}
	}
	return false
}

func terminateAll(t *testing.T, h Harness) {
	t.Helper()
	for _, info := range listOK(t, h) {
		if !info.Running {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
		if err := h.Runner.Terminate(ctx, info.ID); err != nil {
			t.Errorf("Terminate(%q): %v", info.ID, err)
		}
		cancel()
	}
}

func requireNotAvailable(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want errdefs.NotAvailable", what)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("%s: err = %v, want errdefs.NotAvailable", what, err)
	}
}
