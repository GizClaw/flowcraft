package exec

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The tests drive a real process, but not a real *backend*: the starter is this file's, so the module
// does not depend on `sandbox/local` (or on anything else in that module) just to run a command. What
// the tools see is the contract: a starter, and sessions that report output by cursor — which is what
// `sandbox.Exec` reads and what the session tools address.
type fakeStarter struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession
}

func newFakeStarter() *fakeStarter {
	return &fakeStarter{sessions: map[string]*fakeSession{}}
}

func (f *fakeStarter) Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("fake starter: no argv")
	}
	if spec.Opts.WorkDir == "" {
		return nil, errors.New("fake starter: workdir is required")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Opts.WorkDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// One pipe for both streams: the tools reason about one interleaved output, and a test helper that
	// split them would be asserting a shape the contract does not promise.
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &fakeSession{id: spec.ID, pid: cmd.Process.Pid, cmd: cmd, stdin: stdin, output: output}
	f.mu.Lock()
	f.sessions[spec.ID] = s
	f.mu.Unlock()
	s.pump()
	return s, nil
}

type fakeSession struct {
	id     string
	pid    int
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	output io.ReadCloser

	mu     sync.Mutex
	buf    []byte
	ofEOF  bool
	waited bool
	exit   sandbox.SessionExit
}

// pump copies the child's output into the session's own buffer, which is what a cursor reads from.
func (s *fakeSession) pump() {
	go func() {
		_, _ = io.Copy(&bufferWriter{session: s}, s.output)
		s.mu.Lock()
		s.ofEOF = true
		s.mu.Unlock()
	}()
}

type bufferWriter struct{ session *fakeSession }

func (w *bufferWriter) Write(p []byte) (int, error) {
	w.session.mu.Lock()
	w.session.buf = append(w.session.buf, p...)
	w.session.mu.Unlock()
	return len(p), nil
}

func (s *fakeSession) ID() string { return s.id }
func (s *fakeSession) PID() int   { return s.pid }
func (s *fakeSession) Capabilities() sandbox.SessionCapabilities {
	return sandbox.SessionCapabilities{Stdin: true, Signal: true}
}

// Read answers from the cursor: bytes after byte offset afterSeq.
func (s *fakeSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if afterSeq < 0 {
		return sandbox.SessionOutput{}, errors.New("fake session: negative cursor")
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	// The tools read and then report; a session that answered "nothing yet" the instant a command
	// produced output would make every such test racy. Wait briefly for something to appear, then
	// answer with whatever is there.
	if !s.waitForOutput(ctx, afterSeq) {
		return sandbox.SessionOutput{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from := int(afterSeq)
	if from > len(s.buf) {
		from = len(s.buf)
	}
	to := from + maxBytes
	if to > len(s.buf) {
		to = len(s.buf)
	}
	out := sandbox.SessionOutput{NextSeq: int64(to), EOF: s.ofEOF && to == len(s.buf)}
	if to > from {
		out.Chunks = []sandbox.OutputChunk{{
			Seq:    int64(from),
			Stream: sandbox.SessionStreamStdout,
			Data:   append([]byte(nil), s.buf[from:to]...),
		}}
	}
	return out, nil
}

// waitForOutput blocks until there is data past afterSeq, the pipe reached EOF, or the context ends.
func (s *fakeSession) waitForOutput(ctx context.Context, afterSeq int64) bool {
	for {
		s.mu.Lock()
		ready := int64(len(s.buf)) > afterSeq || s.ofEOF
		s.mu.Unlock()
		if ready {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Millisecond):
		}
	}
}

func (s *fakeSession) Write(_ context.Context, data []byte) error {
	_, err := s.stdin.Write(data)
	return err
}

func (s *fakeSession) CloseInput() error { return s.stdin.Close() }

func (s *fakeSession) Resize(context.Context, int, int) error {
	return errdefs.NotAvailablef("fake session: no terminal")
}

func (s *fakeSession) Signal(ctx context.Context, _ sandbox.SessionSignal) error {
	return s.Terminate(ctx)
}

func (s *fakeSession) Terminate(context.Context) error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Kill()
}

// Wait reaps the process once and remembers the verdict, so a caller that asks twice gets the same
// answer instead of a second Wait error from os/exec.
func (s *fakeSession) Wait(ctx context.Context) (sandbox.SessionExit, error) {
	s.mu.Lock()
	if s.waited {
		exit := s.exit
		s.mu.Unlock()
		return exit, nil
	}
	s.mu.Unlock()

	err := s.cmd.Wait()
	exit := sandbox.SessionExit{Reason: sandbox.SessionExited}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit.Code = exitErr.ExitCode()
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			// The OS reported death by signal: a kill is not an ordinary exit, and a caller that
			// cannot tell them apart cannot report "it was stopped" versus "it finished".
			exit.Reason = sandbox.SessionSignaled
			exit.Signal = int(status.Signal())
		}
	default:
		return sandbox.SessionExit{}, err
	}
	s.mu.Lock()
	s.waited = true
	s.exit = exit
	s.mu.Unlock()
	return exit, nil
}

func (s *fakeSession) Watch(context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("fake session: no watcher")
}

func (s *fakeSession) Close() error {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return nil
}
