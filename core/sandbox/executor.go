package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// This file is the one-shot form of the contract: run one command, return its
// output and exit code, own everything it started.
//
// A backend has a single code path — Start — and the one-shot form is a derived
// view over it: drain the session, truncate each stream, report the exit code as
// a result rather than an error. Deriving it here, beside the interface it
// derives from, is what lets a backend implement one thing instead of two.

// execReadWindow is how much one Exec read asks for.
//
// It trades round trips for memory. A local runner reads a slice of the ring
// either way, but a remote one pays one request per window: at 64 KiB a megabyte
// cost sixteen round trips, at 256 KiB it costs four, which measured ~30% off a
// remote Exec of a 1 MiB command. The buffer is small next to what a tool call
// captures anyway.
const execReadWindow = 256 << 10

// defaultMaxOutputBytes is the per-stream cap used by [Exec] when the call names
// none: output beyond it is truncated rather than buffered without bound.
const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

// Exec runs one command to completion through starter and returns its result.
//
// It takes a [SessionStarter] rather than a [Runner] because that is all it uses:
// starting a command is one authority, and enumerating or terminating the
// boundary's sessions is another. A caller that holds only the narrow interface
// can run a command; it cannot reach the rest of the boundary.
//
// The call's deadline covers the whole operation, and every session started is
// closed before Exec returns: nothing outlives the call.
func Exec(
	ctx context.Context,
	starter SessionStarter,
	cmd string,
	args []string,
	opts ExecOptions,
) (*ExecResult, error) {
	if starter == nil {
		return nil, errdefs.Validationf("sandbox: nil starter")
	}

	ctx, span := telemetry.TracerWithSuffix("sandbox").Start(ctx, "sandbox.exec",
		trace.WithAttributes(
			telemetry.String(telemetry.AttrSandboxRunner, fmt.Sprintf("%T", starter)),
		))
	// Who the command belongs to comes from the context, which the host filled at
	// session open; what this layer knows — the runner, the deadline — is added
	// beside it. Capability attributes (the boundary id) arrive with Call.
	span.SetAttributes(telemetry.Attrs(ctx)...)
	defer span.End()
	if opts.Timeout > 0 {
		span.SetAttributes(telemetry.Int64(
			telemetry.AttrSandboxTimeoutMs, opts.Timeout.Milliseconds()))
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	result, err := runCommand(ctx, starter, cmd, args, opts)
	if err != nil {
		// The status label is the classification, never the command or its
		// output: argv can carry a secret, and stderr can echo one back.
		span.SetStatus(codes.Error, execOutcome(err))
		return result, err
	}
	if result == nil {
		// runCommand always produces a result; a nil here would mean the
		// contract changed under the caller rather than a command failing.
		span.SetStatus(codes.Error, "exec_failed")
		return nil, errdefs.Internalf("sandbox: %T returned no result", starter)
	}
	span.SetAttributes(telemetry.Int(telemetry.AttrSandboxExitCode, result.ExitCode))
	span.SetStatus(codes.Ok, "")
	return result, nil
}

// execOutcome classifies a failed command for the span status.
func execOutcome(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case isPolicyDenial(err):
		// The command was refused before anything ran: reporting it as a failed execution would
		// count a rule as a bug.
		return "policy_denied"
	default:
		return "exec_failed"
	}
}

// isPolicyDenial reports whether err is a policy refusal, without importing the policy package's
// typed value: the contract only guarantees the classification through errdefs.
func isPolicyDenial(err error) bool {
	return errdefs.IsPolicyDenied(err)
}

// runCommand drains one session: the derived one-shot form, with the deadline
// already applied by [Exec].
func runCommand(
	ctx context.Context,
	starter SessionStarter,
	cmd string,
	args []string,
	opts ExecOptions,
) (*ExecResult, error) {
	spec := SessionSpec{
		Argv: append([]string{cmd}, args...),
		Opts: opts,
	}
	// One-shot input is known up front, so the spec declares it: a nil
	// or empty Stdin means "no input side" and fd 0 stays /dev/null.
	// Start ignores Opts.Stdin, so this flag is what buys the pipe the
	// Write below fills.
	spec.Stdin = len(opts.Stdin) > 0
	// The one-shot Exec contract is first-max-bytes truncation on the
	// returned result, while the session ring keeps a replayable tail
	// for concurrent watchers. Exec itself drains the ring sequentially
	// from seq 0, so the ring must not trim to the caller's result cap:
	// that would drop bytes before they are read and surface as a
	// sequence gap. Zeroing here lets every built-in runner apply its
	// own default ring cap.
	spec.Opts.Resources.MaxOutputBytes = 0

	sess, err := starter.Start(ctx, spec)
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
		output, err := sess.Read(ctx, afterSeq, execReadWindow)
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
