//go:build windows

package windows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// dialDeadline bounds how long the client waits for the elevated
// helper to appear (UAC prompt + account setup can take a while).
const dialDeadline = 60 * time.Second

func randomPipeName() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return `\\.\pipe\flowcraft-sandbox-` + hex.EncodeToString(b[:])
}

// randomSecret is the per-runner pipe secret, carried to the helper
// via an environment variable and validated on every request.
func randomSecret() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// quoteArg quotes one helper command-line argument (space-safe).
func quoteArg(s string) string {
	if !strings.ContainsAny(s, " \t") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// startElevated is Runner's elevated spawn path: it launches the
// re-executed helper once per Runner (one UAC prompt), then dials the
// pipe for every subsequent session.
func (r *Runner) startElevated(ctx context.Context, spec sandbox.SessionSpec, workDir string, env []string, account string) (sandbox.Session, error) {
	r.elevatedOnce.Do(func() {
		r.elevatedErr = r.launchHelper()
	})
	if r.elevatedErr != nil {
		return nil, r.elevatedErr
	}
	conn, err := r.dialElevated(ctx)
	if err != nil {
		return nil, r.helperError(err)
	}
	r.elevatedMu.Lock()
	secret := r.elevatedSecret
	r.elevatedMu.Unlock()
	req := SpawnRequest{
		Argv:           spec.Argv,
		Cwd:            workDir,
		Env:            env,
		Root:           r.rootDir,
		WritableRoots:  r.writable,
		Account:        account,
		Secret:         secret,
		TTY:            spec.TTY,
		Rows:           spec.Rows,
		Cols:           spec.Cols,
		MaxOutputBytes: spec.Opts.Resources.MaxOutputBytes,
		MemoryBytes:    spec.Opts.Resources.MemoryBytes,
		CPUMillicores:  spec.Opts.Resources.CPUMillicores,
		TimeoutMs:      spec.Opts.Timeout.Milliseconds(),
	}
	sess, err := elevatedSpawn(ctx, conn, spec.ID, req)
	if err != nil {
		return nil, r.helperError(err)
	}
	return sess, nil
}

// helperError enriches a failed dial with the helper's recorded
// startup error, when one exists, so a hidden elevated helper failure
// is not reported as a bare pipe timeout.
func (r *Runner) helperError(err error) error {
	dir, derr := sandboxConfigDir()
	if derr != nil {
		return err
	}
	b, rerr := os.ReadFile(helperLogPath(dir))
	if rerr != nil || len(b) == 0 {
		return err
	}
	tail := strings.TrimSpace(string(b))
	if len(tail) > 512 {
		tail = tail[len(tail)-512:]
	}
	return fmt.Errorf("%w (helper log: %s)", err, tail)
}

// launchHelper re-executes this process elevated with the helper
// marker, carrying a fresh per-runner secret through an environment
// variable that is removed immediately after launch. Each launch uses
// a brand-new pipe name so a still-alive previous helper can never
// block the new one with FILE_FLAG_FIRST_PIPE_INSTANCE.
func (r *Runner) launchHelper() error {
	r.elevatedMu.Lock()
	defer r.elevatedMu.Unlock()
	return r.launchHelperLocked()
}

// launchHelperLocked is launchHelper with r.elevatedMu held.
func (r *Runner) launchHelperLocked() error {
	dir, err := sandboxConfigDir()
	if err != nil {
		return err
	}
	r.elevatedPipe = randomPipeName()
	secret := randomSecret()
	r.elevatedSecret = secret
	// A fresh helper should start with a clean log; stale entries from
	// an earlier launch would point at the wrong failure.
	_ = os.Remove(helperLogPath(dir))
	if err := os.Setenv(helperSecretEnv, secret); err != nil {
		return fmt.Errorf("windows/elevated: set helper secret: %w", err)
	}
	defer func() { _ = os.Unsetenv(helperSecretEnv) }()
	args := HelperArgvMarker + " serve --pipe " + quoteArg(r.elevatedPipe) +
		" --config " + quoteArg(dir) +
		" --root " + quoteArg(r.rootDir)
	if _, err := launchElevated(r.helperExePath(), args, false); err != nil {
		return err
	}
	return nil
}

// dialElevated connects to the helper pipe, relaunching the helper
// once on failure. The relaunch is serialized: concurrent spawns that
// miss the current helper wait for the lock, re-check the (possibly
// refreshed) pipe name, and only relaunch if it is still unreachable.
func (r *Runner) dialElevated(ctx context.Context) (io.ReadWriteCloser, error) {
	r.elevatedMu.Lock()
	pipe := r.elevatedPipe
	r.elevatedMu.Unlock()
	conn, err := dialPipe(ctx, pipe)
	if err == nil {
		return conn, nil
	}

	r.elevatedMu.Lock()
	// Another goroutine may already be relaunching; wait for it to
	// settle instead of firing a second UAC prompt.
	for r.elevatedRel != nil {
		rel := r.elevatedRel
		r.elevatedMu.Unlock()
		select {
		case <-rel:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.elevatedMu.Lock()
	}
	// Re-check the current pipe: the settled relaunch may serve us.
	pipe = r.elevatedPipe
	recheck, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err = dialPipe(recheck, pipe)
	if err == nil {
		r.elevatedMu.Unlock()
		return conn, nil
	}
	// We own the relaunch: publish the settle channel, launch with a
	// fresh pipe name, then close the channel once the dial settles.
	rel := make(chan struct{})
	r.elevatedRel = rel
	if err := r.launchHelperLocked(); err != nil {
		r.elevatedRel = nil
		r.elevatedMu.Unlock()
		return nil, err
	}
	pipe = r.elevatedPipe
	r.elevatedMu.Unlock()
	conn, err = dialPipe(ctx, pipe)
	r.elevatedMu.Lock()
	r.elevatedRel = nil
	close(rel)
	r.elevatedMu.Unlock()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// sendHelperShutdown best-effort tells the elevated helper to exit
// after the Runner closes, so no elevated process outlives the
// runner that launched it.
func (r *Runner) sendHelperShutdown() {
	r.elevatedMu.Lock()
	pipe := r.elevatedPipe
	secret := r.elevatedSecret
	r.elevatedMu.Unlock()
	if pipe == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialPipe(ctx, pipe)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = writeFrame(conn, msgShutdown, ShutdownRequest{Secret: secret})
}

// cleanupSession keeps a proxy (and MITM bundle) alive until the
// wrapped session closes or exits, mirroring seatbelt's sessionHandle.
type cleanupSession struct {
	sandbox.Session
	once    sync.Once
	cleanup func()
}

func (s *cleanupSession) Close() error {
	err := s.Session.Close()
	s.once.Do(s.cleanup)
	return err
}

// elevatedSpawn sends the spawn request on an established connection
// and wraps the resulting stream in a pipeSession proxy.
func elevatedSpawn(ctx context.Context, conn io.ReadWriteCloser, id string, req SpawnRequest) (sandbox.Session, error) {
	if err := writeFrame(conn, msgSpawn, req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("windows/elevated: send spawn request: %w", err)
	}
	kind, payload, err := readFrame(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("windows/elevated: read spawn response: %w", err)
	}
	switch kind {
	case msgReady:
		var ready SpawnReady
		if err := decodePayload(kind, payload, &ready); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return newPipeSession(id, conn, ready, req.MaxOutputBytes), nil
	case msgError:
		var f ErrorFrame
		_ = decodePayload(kind, payload, &f)
		_ = conn.Close()
		return nil, errdefs.Internal(fmt.Errorf("windows/elevated: %s: %s", f.Stage, f.Message))
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("windows/elevated: unexpected response kind %q", kind)
	}
}

// dialPipe retries opening the named pipe until the elevated helper
// publishes it, bounded by ctx and dialDeadline.
func dialPipe(ctx context.Context, pipeName string) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(dialDeadline)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		f, err := os.OpenFile(pipeName, os.O_RDWR, 0)
		if err == nil {
			return f, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("windows/elevated: helper did not start within %s: %w", dialDeadline, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// pipeSession is a sandbox.Session proxy backed by the elevated
// runner's pipe. The server pushes OutputFrame stream; the client
// replays them through outputBuffer with the standard seq semantics.
type pipeSession struct {
	id   string
	conn io.ReadWriteCloser
	out  *outputBuffer
	caps sandbox.SessionCapabilities
	pid  uint32
	done chan struct{}

	connMu    sync.Mutex
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
}

func newPipeSession(id string, conn io.ReadWriteCloser, ready SpawnReady, maxOutputBytes int64) *pipeSession {
	s := &pipeSession{
		id:   id,
		conn: conn,
		out:  newOutputBuffer(maxOutputBytes),
		caps: ready.Caps,
		pid:  ready.PID,
		done: make(chan struct{}),
	}
	// Ctrl-C and Watch are not proxied yet; narrow the capabilities
	// the server reports so callers never rely on NotAvailable paths.
	s.caps.Signal = false
	s.caps.Events = false
	go s.pump()
	return s
}

func (s *pipeSession) pump() {
	defer close(s.done)
	for {
		kind, payload, err := readFrame(s.conn)
		if err != nil {
			// A closed pipe is the client's own Close; the server-side
			// session was terminated, so report SessionTerminated.
			if s.isClosed() {
				s.out.finish(sandbox.SessionExit{Code: -1, Reason: sandbox.SessionTerminated}, nil)
			} else {
				s.out.finish(sandbox.SessionExit{Code: -1, Reason: sandbox.SessionExited}, err)
			}
			return
		}
		switch kind {
		case msgOutput:
			var f OutputFrame
			if decodePayload(kind, payload, &f) != nil {
				continue
			}
			s.out.append(f.Stream, f.Data)
		case msgExit:
			var f ExitFrame
			if decodePayload(kind, payload, &f) != nil {
				continue
			}
			var waitErr error
			if f.Err != "" {
				waitErr = errors.New(f.Err)
			}
			s.out.finish(f.Exit, waitErr)
			return
		case msgError:
			var f ErrorFrame
			if decodePayload(kind, payload, &f) != nil {
				continue
			}
			s.out.finish(sandbox.SessionExit{Code: -1, Reason: sandbox.SessionExited},
				fmt.Errorf("windows/elevated: %s: %s", f.Stage, f.Message))
			return
		}
	}
}

func (s *pipeSession) ID() string { return s.id }

func (s *pipeSession) PID() int { return int(s.pid) }

func (s *pipeSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if s.isClosed() {
		return sandbox.SessionOutput{}, sandbox.ErrSessionClosed
	}
	return s.out.read(ctx, afterSeq, maxBytes)
}

func (s *pipeSession) Write(ctx context.Context, data []byte) error {
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	if len(data) == 0 {
		return nil
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if err := writeFrame(s.conn, msgWrite, WriteRequest{Data: data}); err != nil {
		return errdefs.Internal(fmt.Errorf("windows/elevated: write request: %w", err))
	}
	return nil
}

func (s *pipeSession) CloseInput() error {
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return writeFrame(s.conn, msgCloseInput, struct{}{})
}

func (s *pipeSession) Resize(ctx context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return errdefs.Validationf("windows/elevated: rows and cols must be positive")
	}
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return writeFrame(s.conn, msgResize, ResizeRequest{Rows: rows, Cols: cols})
}

func (s *pipeSession) Signal(context.Context, sandbox.SessionSignal) error {
	return errdefs.NotAvailablef("windows/elevated: signal not supported in P2a")
}

func (s *pipeSession) Terminate(ctx context.Context) error {
	if s.isClosed() {
		return sandbox.ErrSessionClosed
	}
	s.connMu.Lock()
	err := writeFrame(s.conn, msgTerminate, struct{}{})
	s.connMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *pipeSession) Wait(ctx context.Context) (sandbox.SessionExit, error) {
	select {
	case <-s.done:
	case <-ctx.Done():
		return sandbox.SessionExit{}, ctx.Err()
	}
	s.out.mu.Lock()
	defer s.out.mu.Unlock()
	return s.out.exitLocked()
}

func (s *pipeSession) Watch(context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("windows/elevated: watch not supported in P2a")
}

func (s *pipeSession) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.connMu.Lock()
		_ = writeFrame(s.conn, msgClose, struct{}{})
		s.connMu.Unlock()
		_ = s.conn.Close()
	})
	return nil
}

func (s *pipeSession) Capabilities() sandbox.SessionCapabilities {
	return s.caps
}

func (s *pipeSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

var _ sandbox.Session = (*pipeSession)(nil)
