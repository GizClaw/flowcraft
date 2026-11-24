package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// This file covers [sandbox.Exec] — the derived one-shot form — with a scripted
// session instead of a real backend.
//
// It is deliberately not an integration test. What Exec owns is the derivation:
// drain the ring in order, split the streams, truncate per stream, report the
// exit code as a result rather than an error, and close what it started. A real
// process would exercise the backend instead, and the backend is the thing whose
// absence this test is designed to survive.

// scriptedSession replays a fixed chunk list and then reports its exit.
type scriptedSession struct {
	chunks   []sandbox.OutputChunk
	exit     sandbox.SessionExit
	waitFor  time.Duration // blocks Wait for this long, for the timeout case
	stdin    strings.Builder
	closed   bool
	streamed int
}

func (s *scriptedSession) ID() string   { return "scripted" }
func (s *scriptedSession) PID() int     { return 4242 }
func (s *scriptedSession) Close() error { s.closed = true; return nil }

func (s *scriptedSession) Read(ctx context.Context, afterSeq int64, maxBytes int) (sandbox.SessionOutput, error) {
	if err := ctx.Err(); err != nil {
		return sandbox.SessionOutput{}, err
	}
	var out sandbox.SessionOutput
	for _, chunk := range s.chunks {
		if chunk.Seq < afterSeq {
			continue
		}
		out.Chunks = append(out.Chunks, chunk)
		s.streamed++
	}
	out.NextSeq = int64(len(s.chunks))
	out.EOF = true
	return out, nil
}

func (s *scriptedSession) Write(_ context.Context, data []byte) error {
	s.stdin.Write(data)
	return nil
}

func (s *scriptedSession) CloseInput() error { return nil }

func (s *scriptedSession) Resize(context.Context, int, int) error {
	return errdefs.NotAvailablef("resize")
}
func (s *scriptedSession) Signal(context.Context, sandbox.SessionSignal) error {
	return errdefs.NotAvailablef("signal")
}

func (s *scriptedSession) Terminate(context.Context) error { return nil }

func (s *scriptedSession) Wait(ctx context.Context) (sandbox.SessionExit, error) {
	if s.waitFor > 0 {
		select {
		case <-time.After(s.waitFor):
		case <-ctx.Done():
			return sandbox.SessionExit{}, ctx.Err()
		}
	}
	return s.exit, nil
}

func (s *scriptedSession) Watch(context.Context) (sandbox.SessionWatcher, error) {
	return nil, errdefs.NotAvailablef("watch")
}

func (s *scriptedSession) Capabilities() sandbox.SessionCapabilities {
	return sandbox.SessionCapabilities{}
}

// scriptedStarter hands out one session and records the spec it was asked for.
type scriptedStarter struct {
	session *scriptedSession
	spec    sandbox.SessionSpec
	starts  int
}

func (s *scriptedStarter) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	s.starts++
	s.spec = spec
	return s.session, nil
}

func chunk(seq int64, stream sandbox.SessionStream, data string) sandbox.OutputChunk {
	return sandbox.OutputChunk{Seq: seq, Stream: stream, Data: []byte(data)}
}

func TestExecDrainsBothStreamsAndReportsTheExit(t *testing.T) {
	session := &scriptedSession{
		chunks: []sandbox.OutputChunk{
			chunk(0, sandbox.SessionStreamStdout, "out"),
			chunk(1, sandbox.SessionStreamStderr, "err"),
			chunk(2, sandbox.SessionStreamStdout, "-more"),
		},
		exit: sandbox.SessionExit{Code: 3},
	}
	starter := &scriptedStarter{session: session}

	result, err := sandbox.Exec(t.Context(), starter, "sh", []string{"-c", "x"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "out-more" || result.Stderr != "err" {
		t.Fatalf("result = %+v", result)
	}
	// A non-zero exit is a result the caller interprets, never an error.
	if result.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", result.ExitCode)
	}
	if !session.closed {
		t.Fatal("Exec left its session open")
	}
	if starter.spec.Argv[0] != "sh" || len(starter.spec.Argv) != 3 {
		t.Fatalf("Argv = %v", starter.spec.Argv)
	}
}

func TestExecTruncatesPerStream(t *testing.T) {
	session := &scriptedSession{
		chunks: []sandbox.OutputChunk{
			chunk(0, sandbox.SessionStreamStdout, "0123456789"),
			chunk(1, sandbox.SessionStreamStderr, "abcdefghij"),
		},
	}
	result, err := sandbox.Exec(t.Context(), &scriptedStarter{session: session}, "x", nil,
		sandbox.ExecOptions{Resources: policy.Resources{MaxOutputBytes: 4}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "0123" || result.Stderr != "abcd" {
		t.Fatalf("result = %q / %q, want first four bytes of each stream", result.Stdout, result.Stderr)
	}
}

// TestExecDeclaresStdinOnlyWhenItHasSome pins the stdin rule: an empty payload is
// "no input side" (fd 0 stays /dev/null), not "a pipe with nothing in it".
func TestExecDeclaresStdinOnlyWhenItHasSome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin []byte
		want  bool
	}{
		{"nil", nil, false},
		{"empty", []byte{}, false},
		{"some", []byte("hi"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &scriptedSession{}
			starter := &scriptedStarter{session: session}
			if _, err := sandbox.Exec(t.Context(), starter, "cat", nil,
				sandbox.ExecOptions{Stdin: tc.stdin}); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if got := starter.spec.Stdin; got != tc.want {
				t.Fatalf("spec.Stdin = %v, want %v", got, tc.want)
			}
			if got := session.stdin.String(); got != string(tc.stdin) {
				t.Fatalf("written stdin = %q, want %q", got, tc.stdin)
			}
		})
	}
}

func TestExecEnforcesTheCallDeadline(t *testing.T) {
	session := &scriptedSession{waitFor: time.Minute}
	start := time.Now()
	_, err := sandbox.Exec(t.Context(), &scriptedStarter{session: session}, "sleep", nil,
		sandbox.ExecOptions{Timeout: 20 * time.Millisecond})
	if err == nil {
		t.Fatal("Exec returned a result past its deadline")
	}
	if !errdefs.IsTimeout(err) && !isDeadline(err) {
		t.Fatalf("err = %v, want a deadline classification", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Exec waited %v for a 20ms deadline", elapsed)
	}
}

func TestExecRefusesANilStarter(t *testing.T) {
	if _, err := sandbox.Exec(t.Context(), nil, "true", nil, sandbox.ExecOptions{}); err == nil {
		t.Fatal("Exec accepted a nil starter")
	}
}

func isDeadline(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

// TestExecDoesNotShrinkTheSessionRing pins the separation between the two caps:
// the call's MaxOutputBytes truncates the *result*, while the session's ring must
// keep its own default — trimming the ring to the result cap would drop bytes
// before they are read and surface as a sequence gap.
func TestExecDoesNotShrinkTheSessionRing(t *testing.T) {
	starter := &scriptedStarter{session: &scriptedSession{
		chunks: []sandbox.OutputChunk{chunk(0, sandbox.SessionStreamStdout, "0123456789")},
	}}
	result, err := sandbox.Exec(t.Context(), starter, "x", nil,
		sandbox.ExecOptions{Resources: policy.Resources{MaxOutputBytes: 4}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := starter.spec.Opts.Resources.MaxOutputBytes; got != 0 {
		t.Fatalf("the session spec kept MaxOutputBytes=%d; the ring must use its own default", got)
	}
	if result.Stdout != "0123" {
		t.Fatalf("Stdout = %q, want the result truncated to the call's cap", result.Stdout)
	}
}

// TestExecTruncationBoundaries covers the three shapes of the result cap: exact,
// one byte over, and unbounded.
func TestExecTruncationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int64
		want string
	}{
		{"exactly the cap", 10, "0123456789"},
		{"one byte over", 9, "012345678"},
		{"unbounded", 0, "0123456789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starter := &scriptedStarter{session: &scriptedSession{
				chunks: []sandbox.OutputChunk{chunk(0, sandbox.SessionStreamStdout, "0123456789")},
			}}
			result, err := sandbox.Exec(t.Context(), starter, "x", nil,
				sandbox.ExecOptions{Resources: policy.Resources{MaxOutputBytes: tc.cap}})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if result.Stdout != tc.want {
				t.Fatalf("Stdout = %q, want %q", result.Stdout, tc.want)
			}
		})
	}
}

// TestExecReportsCancellation keeps "canceled" and "timed out" apart in the error
// a caller sees, because the two call for different responses: one is the user,
// the other is the deadline.
func TestExecReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := sandbox.Exec(ctx, &scriptedStarter{session: &scriptedSession{}}, "x", nil, sandbox.ExecOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestExecEmitsASpan keeps the one-shot call observable: the span names the
// operation, carries the runner's concrete type and the exit code, and classifies
// a deadline as an error whose status says which failure it was.
func TestExecEmitsASpan(t *testing.T) {
	recorder := telemetrytest.New(t)
	starter := &scriptedStarter{session: &scriptedSession{
		chunks: []sandbox.OutputChunk{chunk(0, sandbox.SessionStreamStdout, "ok")},
		exit:   sandbox.SessionExit{Code: 3},
	}}
	if _, err := sandbox.Exec(t.Context(), starter, "x", nil, sandbox.ExecOptions{}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	span := recorder.RequireNamed(t, "sandbox.exec")
	telemetrytest.RequireAttrString(t, span, telemetry.AttrSandboxRunner, "*sandbox_test.scriptedStarter")
	telemetrytest.RequireAttr(t, span, telemetry.AttrSandboxExitCode, attribute.IntValue(3))
	if got := span.Status().Code; got != codes.Ok && got != codes.Unset {
		t.Fatalf("status = %v, want ok for a non-zero exit that produced a result", got)
	}
}

func TestExecSpanClassifiesADeadline(t *testing.T) {
	recorder := telemetrytest.New(t)
	starter := &scriptedStarter{session: &scriptedSession{waitFor: time.Minute}}
	if _, err := sandbox.Exec(t.Context(), starter, "x", nil,
		sandbox.ExecOptions{Timeout: 20 * time.Millisecond}); err == nil {
		t.Fatal("Exec returned a result past its deadline")
	}
	span := recorder.RequireNamed(t, "sandbox.exec")
	if got := span.Status(); got.Code != codes.Error || got.Description != "timeout" {
		t.Fatalf("status = %v/%q, want Error/timeout", got.Code, got.Description)
	}
}
