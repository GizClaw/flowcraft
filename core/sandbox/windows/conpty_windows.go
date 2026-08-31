//go:build windows

package windows

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	xwin "golang.org/x/sys/windows"
)

// Default pseudo-console window size, in characters. These match the
// unix backend's defaults.
const (
	defaultTTYRows = 24
	defaultTTYCols = 80
)

// conpty wraps a Windows pseudo console (ConPTY): the host-side pipe
// ends (in for input, out for output) and the pseudoconsole handle
// that is passed to the child through the process attribute list.
//
// The child is spawned by [conpty.spawn] with CREATE_SUSPENDED, so the
// caller can assign it to a job object before any user code runs; the
// suspend/resume dance is the same as the pipe session path.
//
// The child's standard handles are wired to the pseudo console by the
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE attribute alone (the official
// Microsoft sample and both mature Go wrappers rely on this), so no
// STARTF_USESTDHANDLES plumbing is needed.
type conpty struct {
	mu       sync.Mutex
	console  xwin.Handle
	attr     *xwin.ProcThreadAttributeListContainer
	in       *os.File // host write end (session stdin)
	out      *os.File // host read end (copy loop)
	released bool
	closed   bool
}

// newConPTY creates the pseudo console with the requested window size
// and the communication pipes. The pipe ends handed to the pseudo
// console are closed here: the console host duplicates them when the
// attached process is created, and keeping them open would prevent the
// output channel from reaching EOF after the session ends.
func newConPTY(rows, cols uint32) (*conpty, error) {
	var ptyIn, inWrite xwin.Handle
	if err := xwin.CreatePipe(&ptyIn, &inWrite, nil, 0); err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: create conpty input pipe: %w", err))
	}
	var outRead, ptyOut xwin.Handle
	if err := xwin.CreatePipe(&outRead, &ptyOut, nil, 0); err != nil {
		_ = xwin.CloseHandle(ptyIn)
		_ = xwin.CloseHandle(inWrite)
		return nil, errdefs.Internal(fmt.Errorf("windows: create conpty output pipe: %w", err))
	}
	closePipes := func() {
		for _, h := range []xwin.Handle{ptyIn, ptyOut, inWrite, outRead} {
			_ = xwin.CloseHandle(h)
		}
	}

	var console xwin.Handle
	if err := xwin.CreatePseudoConsole(
		xwin.Coord{X: int16(cols), Y: int16(rows)}, ptyIn, ptyOut, 0, &console); err != nil {
		closePipes()
		return nil, errdefs.Internal(fmt.Errorf("windows: create pseudo console: %w", err))
	}
	_ = xwin.CloseHandle(ptyIn)
	_ = xwin.CloseHandle(ptyOut)

	attr, err := xwin.NewProcThreadAttributeList(1)
	if err != nil {
		_ = xwin.CloseHandle(console)
		_ = xwin.CloseHandle(inWrite)
		_ = xwin.CloseHandle(outRead)
		return nil, errdefs.Internal(fmt.Errorf("windows: allocate proc thread attribute list: %w", err))
	}
	if err := attr.Update(xwin.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		pseudoconsoleAttrValue(console), unsafe.Sizeof(console)); err != nil {
		attr.Delete()
		_ = xwin.CloseHandle(console)
		_ = xwin.CloseHandle(inWrite)
		_ = xwin.CloseHandle(outRead)
		return nil, errdefs.Internal(fmt.Errorf("windows: set pseudo console attribute: %w", err))
	}

	return &conpty{
		console: console,
		attr:    attr,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
	}, nil
}

// pseudoconsoleAttrValue reinterprets the pseudoconsole handle's bits
// as the attribute value. UpdateProcThreadAttribute expects the HPCON
// value itself in lpValue (Microsoft's sample passes hPC directly),
// not a pointer to it: passing &console makes console apps die at
// startup with STATUS_DLL_INIT_FAILED (0xC0000142).
func pseudoconsoleAttrValue(h xwin.Handle) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&h))
}

// spawn creates a suspended child attached to the pseudo console and
// returns the child's process handle and pid. The caller assigns the
// process to a job and resumes it. The returned process handle is
// owned by the caller.
func (c *conpty) spawn(argv []string, dir string, env []string) (xwin.Handle, int, error) {
	if len(argv) == 0 {
		return 0, 0, errdefs.Validationf("windows: empty argv for tty session")
	}
	// Match os/exec: argv[0] is resolved through PATH up front, and
	// the resolved path becomes the first token of the command line.
	exe, err := exec.LookPath(argv[0])
	if err != nil {
		return 0, 0, errdefs.Internal(fmt.Errorf("windows: resolve %s: %w", argv[0], err))
	}
	cmdline, err := xwin.UTF16PtrFromString(
		xwin.ComposeCommandLine(append([]string{exe}, argv[1:]...)))
	if err != nil {
		return 0, 0, errdefs.Internal(fmt.Errorf("windows: encode command line: %w", err))
	}
	var dirPtr *uint16
	if dir != "" {
		dirPtr, err = xwin.UTF16PtrFromString(dir)
		if err != nil {
			return 0, 0, errdefs.Internal(fmt.Errorf("windows: encode workdir: %w", err))
		}
	}

	// STARTF_USESTDHANDLES with invalid handles stops the child from
	// inheriting the host's stdio; the pseudoconsole attribute then
	// wires the child's standard handles to the pseudo console
	// (portable-pty/wezterm's production pattern).
	siEx := &xwin.StartupInfoEx{
		StartupInfo: xwin.StartupInfo{
			Flags:     xwin.STARTF_USESTDHANDLES,
			StdInput:  xwin.InvalidHandle,
			StdOutput: xwin.InvalidHandle,
			StdErr:    xwin.InvalidHandle,
		},
		ProcThreadAttributeList: c.attr.List(),
	}
	siEx.Cb = uint32(unsafe.Sizeof(*siEx))
	flags := uint32(xwin.CREATE_SUSPENDED | xwin.EXTENDED_STARTUPINFO_PRESENT)
	var envBlock *uint16
	if env != nil {
		// An empty (non-nil) env means "inherit nothing", matching
		// exec.Cmd semantics; nil inherits the parent environment.
		envBlock = buildEnvBlock(dedupEnvCase(env))
		flags |= xwin.CREATE_UNICODE_ENVIRONMENT
	}

	var pi xwin.ProcessInformation
	if err := xwin.CreateProcess(nil, cmdline, nil, nil, false, flags,
		envBlock, dirPtr, &siEx.StartupInfo, &pi); err != nil {
		return 0, 0, classifyStartError(argv[0], err)
	}
	_ = xwin.CloseHandle(pi.Thread)
	return pi.Process, int(pi.ProcessId), nil
}

// resize updates the pseudo console window size.
func (c *conpty) resize(rows, cols uint32) error {
	c.mu.Lock()
	console := c.console
	c.mu.Unlock()
	if console == 0 {
		return sandbox.ErrSessionClosed
	}
	if err := xwin.ResizePseudoConsole(console, xwin.Coord{X: int16(cols), Y: int16(rows)}); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: resize pseudo console: %w", err))
	}
	return nil
}

// releaseConsole closes the pseudo console and frees the attribute
// list. The console host emits a final output frame and closes the
// output channel, so callers should keep draining [conpty.out] until
// EOF afterwards.
func (c *conpty) releaseConsole() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.released {
		return
	}
	c.released = true
	c.attr.Delete()
	xwin.ClosePseudoConsole(c.console)
	c.console = 0
}

// close releases the console and both host-side pipe handles. It is
// idempotent and safe to race with the output copy loop: closing the
// read end unblocks a stuck read with an error.
func (c *conpty) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if !c.released {
		c.released = true
		c.attr.Delete()
		xwin.ClosePseudoConsole(c.console)
		c.console = 0
	}
	in, out := c.in, c.out
	c.mu.Unlock()
	return errors.Join(in.Close(), out.Close())
}

// ttyExitError is the TTY counterpart of *exec.ExitError: it carries
// the child's exit code into the shared exit classifier without
// needing an os.ProcessState.
type ttyExitError struct {
	code int
}

func (e *ttyExitError) Error() string {
	return fmt.Sprintf("windows: tty process exited with code %d", e.code)
}

func (e *ttyExitError) ExitCode() int { return e.code }

// buildEnvBlock converts env into a UTF-16, double-NUL terminated
// environment block for CreateProcess.
func buildEnvBlock(env []string) *uint16 {
	var b strings.Builder
	for _, kv := range env {
		b.WriteString(kv)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	u := utf16.Encode([]rune(b.String()))
	return &u[0]
}

// dedupEnvCase drops duplicate environment keys case-insensitively,
// keeping the last occurrence, which is what os/exec does for
// Windows children (the environment is case-insensitive there).
func dedupEnvCase(env []string) []string {
	last := make(map[string]int, len(env))
	for i, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		last[strings.ToUpper(key)] = i
	}
	out := make([]string, 0, len(last))
	for i, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if last[strings.ToUpper(key)] == i {
			out = append(out, kv)
		}
	}
	return out
}
