//go:build windows

package windows

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	"golang.org/x/sys/windows"
)

const (
	// pipeInstances is the number of named-pipe instances the helper
	// serves concurrently, so parallel sessions from one Runner never
	// serialize behind a single connection.
	pipeInstances = 64
	// helperIdleTimeout is how long the elevated helper stays alive
	// after its last connection before exiting cleanly, so an idle
	// agent does not leave an orphan elevated process behind.
	helperIdleTimeout = 10 * time.Minute
)

// errShutdown signals a clean helper shutdown request from the Runner.
var errShutdown = errors.New("windows/elevated: shutdown requested")

var (
	procConvertStringSecurityDescriptorToSecurityDescriptorW = modadvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

// SandboxHelperServe runs the elevated half of the P2 backend: it
// ensures the sandbox accounts + WFP filters exist (this process is
// elevated), then serves spawn requests on pipeName concurrently
// (pipeInstances instances). It exits on ctx cancellation, on an
// explicit shutdown frame from the Runner, or after helperIdleTimeout
// with no activity. root is the only workspace root the server will
// accept ACL work for; secret must match the per-runner value the
// client put in every request.
func SandboxHelperServe(ctx context.Context, pipeName, configDir, root, secret string) error {
	if !setupComplete(configDir) {
		if err := SandboxHelperInstall(configDir); err != nil {
			return err
		}
	}
	if secret == "" {
		return fmt.Errorf("windows/elevated: helper secret missing from environment")
	}

	sa, freeSD, err := pipeSecurityAttributes()
	if err != nil {
		return err
	}
	defer freeSD()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	activity := &pipeActivity{}
	activity.touch()

	var wg sync.WaitGroup
	errc := make(chan error, pipeInstances)
	for i := 0; i < pipeInstances; i++ {
		wg.Add(1)
		go func(first bool) {
			defer wg.Done()
			if err := serveInstance(ctx, pipeName, configDir, root, secret, sa, first, activity); err != nil {
				if !errors.Is(err, errShutdown) {
					errc <- err
				}
				cancel()
			}
		}(i == 0)
	}

	idle := time.NewTicker(30 * time.Second)
	defer idle.Stop()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		select {
		case <-ctx.Done():
			<-done
			select {
			case err := <-errc:
				return err
			default:
				return nil
			}
		case <-done:
			select {
			case err := <-errc:
				return err
			default:
			}
			return nil
		case <-idle.C:
			if activity.idleFor(helperIdleTimeout) {
				cancel()
			}
		}
	}
}

// pipeActivity tracks helper liveness: while a connection is in
// flight the helper must never idle-cancel (long builds, silent
// interactive shells), and frames refresh the timestamp so churn
// keeps the helper warm.
type pipeActivity struct {
	last   atomic.Int64
	active atomic.Int64
}

func (a *pipeActivity) touch() { a.last.Store(time.Now().Unix()) }

func (a *pipeActivity) incr() { a.active.Add(1) }

func (a *pipeActivity) decr() { a.active.Add(-1) }

func (a *pipeActivity) idleFor(d time.Duration) bool {
	return a.active.Load() == 0 &&
		time.Since(time.Unix(a.last.Load(), 0)) > d
}

// serveInstance owns one pipe-instance slot: create, wait for a
// client, serve one session, close, repeat. The very first instance
// for the name carries FILE_FLAG_FIRST_PIPE_INSTANCE.
func serveInstance(
	ctx context.Context,
	pipeName, configDir, root, secret string,
	sa *windows.SecurityAttributes,
	first bool,
	activity *pipeActivity,
) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		flags := uint32(windows.PIPE_ACCESS_DUPLEX)
		if first {
			flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
			first = false
		}
		h, err := windows.CreateNamedPipe(
			mustUTF16Ptr(pipeName),
			flags,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
			pipeInstances,
			0x10000,
			0x10000,
			0,
			sa,
		)
		if err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(200 * time.Millisecond):
					continue
				}
			}
			return fmt.Errorf("windows/elevated: create pipe: %w", err)
		}
		if err := windows.ConnectNamedPipe(h, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			_ = windows.CloseHandle(h)
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		activity.touch()
		activity.incr()
		f := os.NewFile(uintptr(h), pipeName)
		err = serveConn(ctx, f, configDir, root, secret, activity)
		_ = f.Close()
		activity.decr()
		activity.touch()
		if errors.Is(err, errShutdown) {
			return errShutdown
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			telemetry.Warn(context.Background(),
				"windows/elevated: session connection failed",
				otellog.String("windows.pipe", pipeName))
		}
	}
}

func mustUTF16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

// serveConn handles one client connection: validate the request
// (secret, account, workspace-root bounds), log on the requested
// account, spawn through the shared windows session implementation,
// then stream output and forward controls.
func serveConn(ctx context.Context, conn io.ReadWriteCloser, configDir, root, secret string, activity *pipeActivity) error {
	kind, payload, err := readFrame(conn)
	if err != nil {
		return err
	}
	activity.touch()
	switch kind {
	case msgShutdown:
		var req ShutdownRequest
		_ = decodePayload(kind, payload, &req)
		if req.Secret == "" || req.Secret != secret {
			return fmt.Errorf("windows/elevated: shutdown with bad secret")
		}
		return errShutdown
	case msgSpawn:
		// fall through
	default:
		return fmt.Errorf("windows/elevated: expected spawn or shutdown, got %q", kind)
	}
	var req SpawnRequest
	if err := decodePayload(kind, payload, &req); err != nil {
		return err
	}
	if err := validateSpawnRequest(&req, root, secret); err != nil {
		_ = sendError(conn, "spawn", err)
		return nil
	}
	if len(req.Argv) == 0 {
		_ = sendError(conn, "spawn", errdefs.Validationf("windows/elevated: SpawnRequest.Argv must name a command"))
		return nil
	}

	creds, err := loadCreds(configDir)
	if err != nil {
		_ = sendError(conn, "spawn", err)
		return nil
	}
	acct := creds.Online
	if req.Account == sandboxAccountOffline {
		acct = creds.Offline
	}
	if acct == nil {
		_ = sendError(conn, "spawn", errdefs.Internalf("windows/elevated: missing credentials for account %q", req.Account))
		return nil
	}
	tok, err := logonSandboxUser(acct)
	if err != nil {
		_ = sendError(conn, "spawn", err)
		return nil
	}
	defer func() { _ = tok.Close() }()

	sess, err := spawnAsSandboxUser(ctx, &req, tok, acct.Username)
	if err != nil {
		_ = sendError(conn, "spawn", err)
		return nil
	}
	if err := writeFrame(conn, msgReady, SpawnReady{
		PID:  uint32(sess.PID()),
		Caps: sess.Capabilities(),
	}); err != nil {
		_ = sess.Close()
		return err
	}
	return runSession(ctx, conn, sess, activity)
}

// validateSpawnRequest refuses anything the client must not control:
// the pipe secret must match, the account must be one of the two
// sandbox accounts, and every path the elevated process would touch
// (Root, Cwd, WritableRoots) must stay inside the launch-time root.
// The elevated helper must never trust the unelevated client for the
// ACL tampering surface (applyAccountACLs).
func validateSpawnRequest(req *SpawnRequest, root, secret string) error {
	if req.Secret == "" || req.Secret != secret {
		return errdefs.Forbiddenf("windows/elevated: bad helper secret")
	}
	if req.Account != sandboxAccountOffline && req.Account != sandboxAccountOnline {
		return errdefs.Validationf("windows/elevated: unknown account %q", req.Account)
	}
	if !pathWithinRoot(req.Root, root) || !pathWithinRoot(req.Cwd, root) {
		return errdefs.Forbiddenf("windows/elevated: workspace root or cwd escapes the launch root")
	}
	for _, w := range req.WritableRoots {
		if !pathWithinRoot(w, root) {
			return errdefs.Forbiddenf("windows/elevated: writable root %q escapes the launch root", w)
		}
	}
	return nil
}

// pathWithinRoot reports whether p is root or a descendant, using the
// case-insensitive comparison Windows path semantics need.
func pathWithinRoot(p, root string) bool {
	clean := filepath.Clean(p)
	r := filepath.Clean(root)
	return strings.EqualFold(clean, r) ||
		strings.HasPrefix(strings.ToLower(clean), strings.ToLower(r)+strings.ToLower(string(filepath.Separator)))
}

// spawnAsSandboxUser builds the configured command with the sandbox
// account's token and hands it to core/sandbox.StartWindowsSession,
// which owns stdio (ConPTY/pipes), the job object, and reaping. The
// token's default DACL is set so child-created objects stay usable.
func spawnAsSandboxUser(ctx context.Context, req *SpawnRequest, tok windows.Token, username string) (sandbox.Session, error) {
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = req.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(tok)}

	sid, err := accountSID(username)
	if err != nil {
		return nil, err
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return nil, err
	}
	if err := setTokenDefaultDACL(tok, []*windows.SID{sid, everyone}); err != nil {
		return nil, err
	}
	// Grant the sandbox account write access to the workspace roots
	// and protect the agent-owned subdirectories, so the child (which
	// runs as a different user) can work and still cannot rewrite its
	// own policy.
	if err := applyAccountACLs(req, sid); err != nil {
		return nil, err
	}

	spec := sandbox.SessionSpec{
		Argv: req.Argv,
		TTY:  req.TTY,
		Rows: req.Rows,
		Cols: req.Cols,
		Opts: sandbox.ExecOptions{
			Resources: sandbox.ResourceLimits{
				MemoryBytes:    req.MemoryBytes,
				CPUMillicores:  req.CPUMillicores,
				MaxOutputBytes: req.MaxOutputBytes,
			},
			Timeout: durationMs(req.TimeoutMs),
		},
	}
	return sandbox.StartWindowsSession(ctx, spec, cmd)
}

// applyAccountACLs grants sid write access on root + writable roots
// and adds deny-write ACEs for cwd's protected subdirectories. The
// elevated process can do this for the account because the files are
// owned by the same user that elevated. Callers must validate the
// paths first (validateSpawnRequest).
func applyAccountACLs(req *SpawnRequest, sid *windows.SID) error {
	seen := map[string]bool{}
	for _, p := range append([]string{req.Root}, req.WritableRoots...) {
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if _, err := ensureAllowWriteACE(clean, sid); err != nil {
			return err
		}
	}
	return applyProtectedDeniesSID(req.Cwd, sid)
}

// runSession pumps output to the client and handles control frames
// until the client disconnects or the session ends.
func runSession(ctx context.Context, conn io.ReadWriteCloser, sess sandbox.Session, activity *pipeActivity) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	send := func(kind string, payload any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		activity.touch()
		return writeFrame(conn, kind, payload)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		after := int64(0)
		for {
			out, err := sess.Read(ctx, after, 64*1024)
			if err != nil {
				if ctx.Err() != nil {
					// Server-side cancellation (shutdown): never leave
					// the client waiting for an exit frame.
					_ = send(msgExit, ExitFrame{
						Exit: sandbox.SessionExit{Code: -1, Reason: sandbox.SessionTerminated},
					})
					return
				}
				_ = send(msgError, ErrorFrame{Stage: "read", Message: err.Error()})
				return
			}
			for _, ch := range out.Chunks {
				if err := send(msgOutput, OutputFrame{Seq: ch.Seq, Stream: ch.Stream, Data: ch.Data}); err != nil {
					return
				}
			}
			after = out.NextSeq
			if out.EOF {
				break
			}
		}
		exit, err := sess.Wait(ctx)
		_ = send(msgExit, ExitFrame{Exit: exit, Err: errString(err)})
	}()

	for {
		kind, payload, err := readFrame(conn)
		if err != nil {
			break
		}
		activity.touch()
		switch kind {
		case msgWrite:
			var req WriteRequest
			if decodePayload(kind, payload, &req) != nil {
				continue
			}
			if err := sess.Write(ctx, req.Data); err != nil {
				_ = send(msgError, ErrorFrame{Stage: "write", Message: err.Error()})
			}
		case msgResize:
			var req ResizeRequest
			if decodePayload(kind, payload, &req) != nil {
				continue
			}
			if err := sess.Resize(ctx, req.Rows, req.Cols); err != nil {
				_ = send(msgError, ErrorFrame{Stage: "resize", Message: err.Error()})
			}
		case msgCloseInput:
			if err := sess.CloseInput(); err != nil {
				_ = send(msgError, ErrorFrame{Stage: "close_input", Message: err.Error()})
			}
		case msgTerminate:
			if err := sess.Terminate(ctx); err != nil {
				_ = send(msgError, ErrorFrame{Stage: "terminate", Message: err.Error()})
			}
		case msgClose:
			cancel()
			_ = sess.Close()
			wg.Wait()
			return nil
		}
	}
	cancel()
	_ = sess.Close()
	wg.Wait()
	return nil
}

func sendError(conn io.Writer, stage string, err error) error {
	return writeFrame(conn, msgError, ErrorFrame{Stage: stage, Message: err.Error()})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func durationMs(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// pipeSecurityAttributes builds a SECURITY_ATTRIBUTES whose DACL
// admits only SYSTEM, Administrators, and the current user, so other
// users (and low-integrity processes) cannot connect to the helper
// pipe. The returned free function releases the security descriptor.
func pipeSecurityAttributes() (*windows.SecurityAttributes, func(), error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, nil, fmt.Errorf("windows/elevated: open process token: %w", err)
	}
	defer tok.Close()
	user, err := tok.GetTokenUser()
	if err != nil {
		return nil, nil, fmt.Errorf("windows/elevated: get token user: %w", err)
	}
	userSID := user.User.Sid.String()
	sddl := fmt.Sprintf("D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)", userSID)
	sddlPtr, err := windows.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, nil, err
	}
	var sd uintptr
	var size uint32
	r1, _, e1 := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		1, // SECURITY_DESCRIPTOR_REVISION
		uintptr(unsafe.Pointer(&sd)),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return nil, nil, fmt.Errorf("windows/elevated: build pipe security descriptor: %w", e1)
	}
	sdPtr := *(*unsafe.Pointer)(unsafe.Pointer(&sd))
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: (*windows.SECURITY_DESCRIPTOR)(sdPtr),
		InheritHandle:      0,
	}
	return sa, func() {
		if sd != 0 {
			_, _ = windows.LocalFree(windows.Handle(sd))
		}
	}, nil
}
