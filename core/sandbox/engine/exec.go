package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// defaultMaxOutputBytes is the per-stream cap used by the one-shot Exec helper:
// output beyond it is truncated rather than buffered without bound.
const defaultMaxOutputBytes int64 = 10 * 1024 * 1024

func Exec(
	ctx context.Context,
	runner sandbox.Runner,
	cmd string,
	args []string,
	opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	if runner == nil {
		return nil, errdefs.Validationf("sandbox: nil runner")
	}

	ctx, span := telemetry.TracerWithSuffix("sandbox").Start(ctx, "sandbox.exec",
		trace.WithAttributes(
			attribute.String(telemetry.AttrSandboxRunner, fmt.Sprintf("%T", runner)),
		))
	defer span.End()
	if opts.Timeout > 0 {
		span.SetAttributes(attribute.Int64(
			telemetry.AttrSandboxTimeoutMs, opts.Timeout.Milliseconds()))
	}

	result, err := runCommand(ctx, runner, cmd, args, opts)
	if err != nil {
		// The status label is the classification, never the command or its
		// output: argv can carry a secret, and stderr can echo one back.
		span.SetStatus(codes.Error, execOutcome(err))
		return result, err
	}
	span.SetAttributes(attribute.Int(
		telemetry.AttrSandboxExitCode, result.ExitCode))
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
	default:
		return "exec_failed"
	}
}

// runCommand is the uninstrumented one-shot execution.
func runCommand(
	ctx context.Context,
	runner sandbox.Runner,
	cmd string,
	args []string,
	opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	spec := sandbox.SessionSpec{
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
			case sandbox.SessionStreamStdout:
				_, _ = stdout.Write(chunk.Data)
			case sandbox.SessionStreamStderr:
				_, _ = stderr.Write(chunk.Data)
			}
		}
		afterSeq = output.NextSeq
		if output.EOF {
			break
		}
	}

	exit, err := sess.Wait(ctx)
	result := &sandbox.ExecResult{
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
