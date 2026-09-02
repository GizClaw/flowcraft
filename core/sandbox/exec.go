package sandbox

import (
	"bytes"
	"context"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// defaultMaxOutputBytes is the per-stream cap used by the one-shot Exec
// when ExecOptions.Resources.MaxOutputBytes is zero.
const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// ExecOptions configures one Runner.Exec call.
//
// Field semantics:
//
//   - WorkDir: directory the command runs in. Relative paths are resolved
//     against the runner's root (e.g. local.Runner.rootDir); absolute paths
//     must stay inside the root or the call is rejected with
//     ErrPathTraversal. Empty means "use the runner's root".
//   - Stdin: bytes piped to the command's stdin. nil means no stdin.
//   - Timeout: per-call deadline. Zero means "no sandbox-imposed timeout"
//     (the caller's ctx still applies).
//   - Env: see EnvPolicy. Replaces the historical "inherit everything"
//     behaviour while staying back-compat when EnvPolicy.Allow is nil.
//   - Net: see NetPolicy. The local runner only accepts NetDefault.
//   - Write: see WritePolicy. Zero value keeps the runner's
//     construction-time write boundary; WriteReadOnly narrows it for
//     this call only (root not writable, explicit writable paths stay).
//   - Resources: see ResourceLimits. The local runner enforces MemoryBytes,
//     CPUMillicores (with Timeout), and MaxOutputBytes; DiskBytes is
//     still errdefs.NotAvailable.
type ExecOptions struct {
	WorkDir   string
	Stdin     []byte
	Timeout   time.Duration
	Env       EnvPolicy
	Net       corenet.NetPolicy
	Write     WritePolicy
	Resources ResourceLimits
}

// ExecResult captures the outcome of a Runner.Exec call. ExitCode is the
// command's exit status (0 on success, non-zero on failure that the OS
// surfaced via *exec.ExitError or equivalent). Stdout / Stderr are the
// captured output, possibly truncated to Resources.MaxOutputBytes.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Exec is the one-shot view of a sandbox: it runs cmd to completion
// through the runner's session capability. The command is started as a
// non-TTY process, stdin is written and closed, stdout/stderr are
// collected (truncated to the first MaxOutputBytes per stream, the
// same semantics as the former local Runner.Exec), and the exit code is
// surfaced on ExecResult. Non-zero exits return err == nil.
//
// Policy enforcement (net posture, workdir confinement, resource
// watcher, timeout) belongs to the runner's Start implementation, so
// every backend gets Exec for free. Exec intentionally passes a zero
// MaxOutputBytes to Start so the session ring is bounded by the
// runner's configured default (10 MiB for the built-in runners) while
// the returned result is truncated to the caller's cap. Custom
// Runners must apply the same default before calling StartSession,
// otherwise a non-positive MaxOutputBytes leaves the replay ring
// unbounded.
func Exec(
	ctx context.Context,
	runner Runner,
	cmd string,
	args []string,
	opts ExecOptions,
) (*ExecResult, error) {
	if runner == nil {
		return nil, errdefs.Validationf("sandbox: nil runner")
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	spec := SessionSpec{
		Argv: append([]string{cmd}, args...),
		Opts: opts,
	}
	// The one-shot Exec contract is first-max-bytes truncation on the
	// returned result, while the session ring keeps a replayable tail
	// for concurrent watchers. Exec itself drains the ring sequentially
	// from seq 0, so the ring must not trim to the caller's result cap:
	// that would drop bytes before they are read and surface as a
	// sequence gap. Zeroing here lets every built-in runner apply its
	// own default ring cap (see local.WithMaxOutputBytes and friends).
	spec.Opts.Resources.MaxOutputBytes = 0

	sess, err := runner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			telemetry.WarnErr(ctx, "sandbox: close session after exec failed", cerr)
		}
	}()

	if len(opts.Stdin) > 0 {
		if err := sess.Write(ctx, opts.Stdin); err != nil {
			return nil, err
		}
	}
	if err := sess.CloseInput(); err != nil {
		return nil, err
	}

	maxOut := opts.Resources.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutputBytes
	}
	var stdout, stderr limitedBuffer
	stdout.max = maxOut
	stderr.max = maxOut

	afterSeq := int64(0)
	for {
		output, err := sess.Read(ctx, afterSeq, 64*1024)
		if err != nil {
			return nil, errdefs.FromContext(err)
		}
		for _, chunk := range output.Chunks {
			switch chunk.Stream {
			case SessionStreamStdout:
				_, _ = stdout.Write(chunk.Data)
			case SessionStreamStderr:
				_, _ = stderr.Write(chunk.Data)
			}
		}
		afterSeq = output.NextSeq
		if output.EOF {
			break
		}
	}

	exit, err := sess.Wait(ctx)
	result := &ExecResult{
		Stdout: stdout.buf.String(),
		Stderr: stderr.buf.String(),
	}
	if err != nil {
		return result, errdefs.FromContext(err)
	}
	if ctx.Err() != nil {
		return result, errdefs.FromContext(ctx.Err())
	}
	result.ExitCode = exit.Code
	return result, nil
}

// limitedBuffer keeps the first max bytes written; excess is dropped
// silently, matching the one-shot Exec output contract.
type limitedBuffer struct {
	buf bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 || int64(b.buf.Len()) >= b.max {
		return len(p), nil
	}
	space := b.max - int64(b.buf.Len())
	if int64(len(p)) <= space {
		return b.buf.Write(p)
	}
	if _, err := b.buf.Write(p[:space]); err != nil {
		return 0, err
	}
	return len(p), nil
}
