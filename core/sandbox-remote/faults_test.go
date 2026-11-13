package remote_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// The fault layer: what the remote stack does when the network, the server or the
// client goes away mid-operation. None of these paths are reachable from the
// conformance suite, which assumes a working connection throughout.

// flakyTransport wraps a real transport and injects failures, counting attempts
// per operation so "retried" and "not retried" are observable facts.
type flakyTransport struct {
	inner remote.Transport

	mu       sync.Mutex
	failNext map[remote.Op]int
	attempts map[remote.Op]int
}

func newFlaky(inner remote.Transport) *flakyTransport {
	return &flakyTransport{
		inner:    inner,
		failNext: make(map[remote.Op]int),
		attempts: make(map[remote.Op]int),
	}
}

func (f *flakyTransport) Do(ctx context.Context, op remote.Op, req []byte, res any) error {
	if err := f.gate(op); err != nil {
		return err
	}
	return f.inner.Do(ctx, op, req, res)
}

func (f *flakyTransport) DoBytes(ctx context.Context, op remote.Op, req []byte) ([]byte, error) {
	if err := f.gate(op); err != nil {
		return nil, err
	}
	return f.inner.DoBytes(ctx, op, req)
}

func (f *flakyTransport) Stream(ctx context.Context, op remote.Op, req []byte) (io.ReadCloser, error) {
	if err := f.gate(op); err != nil {
		return nil, err
	}
	return f.inner.Stream(ctx, op, req)
}

// gate counts the attempt and fails it when the test asked for that.
func (f *flakyTransport) gate(op remote.Op) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[op]++
	if f.failNext[op] > 0 {
		f.failNext[op]--
		return &remote.TransportError{Err: errors.New("injected transport failure")}
	}
	return nil
}

func (f *flakyTransport) fail(op remote.Op, count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[op] = count
}

func (f *flakyTransport) attemptCount(op remote.Op) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[op]
}

// remoteWithTransport builds local -> wire -> flaky client.
func remoteWithTransport(t *testing.T, wrap func(remote.Transport) *flakyTransport, clientOpts ...remote.Option) (*remote.Runner, *flakyTransport) {
	t.Helper()
	root := t.TempDir()

	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithServerToken(testToken))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)

	base := remote.NewHTTPTransport(listener.URL, remote.WithHTTPToken(testToken))
	flaky := wrap(base)
	opts := append([]remote.Option{remote.WithTransport(flaky)}, clientOpts...)
	// A context here only bounds the handshake, so give it room.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := remote.New(ctx, listener.URL, opts...)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, flaky
}

// A transport failure is recoverable for operations that are safe to repeat, and
// fatal for the one that is not. This is the rule the client documents; here it
// is as an attempt count.
func TestRetryOnlyRepeatsSafeOperations(t *testing.T) {
	client, flaky := remoteWithTransport(t, newFlaky)

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo hi"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	t.Run("read retries", func(t *testing.T) {
		flaky.fail(remote.OpRead, 2)
		before := flaky.attemptCount(remote.OpRead)
		out, err := sess.Read(context.Background(), 0, 64)
		if err != nil {
			t.Fatalf("Read = %v, want a retried success", err)
		}
		if !strings.Contains(concat(out.Chunks), "hi") {
			t.Fatalf("output = %q", concat(out.Chunks))
		}
		if got := flaky.attemptCount(remote.OpRead) - before; got != 3 {
			t.Fatalf("Read attempts = %d, want 3 (two failures plus the success)", got)
		}
	})

	t.Run("list retries", func(t *testing.T) {
		flaky.fail(remote.OpList, 1)
		before := flaky.attemptCount(remote.OpList)
		if _, err := client.List(context.Background()); err != nil {
			t.Fatalf("List = %v, want a retried success", err)
		}
		if got := flaky.attemptCount(remote.OpList) - before; got != 2 {
			t.Fatalf("List attempts = %d, want 2", got)
		}
	})

	t.Run("read gives up after the retry budget", func(t *testing.T) {
		flaky.fail(remote.OpRead, 99)
		before := flaky.attemptCount(remote.OpRead)
		if _, err := sess.Read(context.Background(), 0, 64); err == nil {
			t.Fatal("Read succeeded with every attempt failing")
		}
		// defaultRetries is 3, so four attempts: the first plus three retries.
		if got := flaky.attemptCount(remote.OpRead) - before; got != 4 {
			t.Fatalf("Read attempts = %d, want 4", got)
		}
		flaky.fail(remote.OpRead, 0)
	})

	// A write is never repeated: the bytes may already have reached the child,
	// and a duplicate "rm -rf" is not recoverable.
	t.Run("write is not retried", func(t *testing.T) {
		sess, err := client.Start(context.Background(), sandbox.SessionSpec{
			Argv:  []string{"cat"},
			Stdin: true,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = sess.Close() }()

		flaky.fail(remote.OpWrite, 5)
		before := flaky.attemptCount(remote.OpWrite)
		if err := sess.Write(context.Background(), []byte("once")); err == nil {
			t.Fatal("Write reported success on an injected transport failure")
		}
		if got := flaky.attemptCount(remote.OpWrite) - before; got != 1 {
			t.Fatalf("Write attempts = %d, want exactly 1", got)
		}
		flaky.fail(remote.OpWrite, 0)
	})
}

// A held read that times out on the server is an idle batch, not an error and not
// a cursor jump: the caller simply asks again.
func TestHeldReadReturnsIdleWithoutMovingTheCursor(t *testing.T) {
	root := t.TempDir()
	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	// The server caps the hold below what the client asks for, which is the
	// configuration a proxy with a shorter idle timeout forces.
	server, err := remote.NewServer(inner,
		remote.WithServerToken(testToken),
		remote.WithMaxHold(150*time.Millisecond))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)

	client, err := remote.New(context.Background(), listener.URL,
		remote.WithToken(testToken),
		remote.WithPollHold(300*time.Millisecond))
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "sleep 0.4; echo late"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := sess.Read(ctx, 0, 64)
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Chunks) != 0 || first.NextSeq != 0 {
		t.Fatalf("first Read = %+v, want an idle batch at the same cursor", first)
	}

	// Keep asking; the output arrives and the cursor advances only then.
	var output []byte
	after := first.NextSeq
	for {
		out, err := sess.Read(ctx, after, 64)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, c := range out.Chunks {
			output = append(output, c.Data...)
		}
		after = out.NextSeq
		if out.EOF {
			break
		}
	}
	if !strings.Contains(string(output), "late") {
		t.Fatalf("output = %q", output)
	}
}

// Cancelling while the server holds the request is a cancellation, not an empty
// batch: a caller that mistook it for idle would spin.
func TestCancelDuringHoldIsACancellation(t *testing.T) {
	root := t.TempDir()
	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithServerToken(testToken))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)

	client, err := remote.New(context.Background(), listener.URL,
		remote.WithToken(testToken),
		remote.WithPollHold(5*time.Second))
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := sess.Read(ctx, 0, 64)
	if err == nil {
		t.Fatalf("Read returned %+v after the caller's context ended", out)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errdefs.IsTimeout(err) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Read took %v; the caller's 100ms context did not bound it", elapsed)
	}
}

// A push consumer that reads slowly must lose nothing: the pump blocks on a full
// queue rather than dropping frames.
func TestSlowEventConsumerLosesNothing(t *testing.T) {
	const frames = 200

	client, err := remote.New(context.Background(), "http://unused",
		remote.WithTransport(&pipeTransport{t: t, frames: frames}))
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer func() { _ = client.Close() }()

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	watcher, err := sess.Watch(context.Background())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = watcher.Close() }()

	var (
		seqs  []int64
		exits int
	)
	deadline := time.After(20 * time.Second)
	for len(seqs) < frames || exits == 0 {
		select {
		case ev, open := <-watcher.Events():
			if !open {
				if len(seqs) != frames || exits == 0 {
					t.Fatalf("stream closed early: %d/%d events, %d exits", len(seqs), frames, exits)
				}
				return
			}
			switch ev.Type {
			case sandbox.SessionEventOutput:
				seqs = append(seqs, ev.Seq)
			case sandbox.SessionEventExited:
				exits++
			}
		case <-deadline:
			t.Fatalf("timed out: %d/%d events, %d exits", len(seqs), frames, exits)
		}
	}
	for i, seq := range seqs {
		if seq != int64(i) {
			t.Fatalf("event %d has seq %d; a frame was dropped or reordered", i, seq)
		}
	}
}

// Cancel closes the subscription's channel even when the pump is blocked on a
// full queue.
func TestWatchCancelClosesTheChannel(t *testing.T) {
	client, err := remote.New(context.Background(), "http://unused",
		remote.WithTransport(&pipeTransport{t: t, frames: 1000}))
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer func() { _ = client.Close() }()

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"sh"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := sess.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, open := <-watcher.Events():
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the channel stayed open after the subscription was cancelled")
		}
	}
}

// A restarted server has no sessions: the ids it minted are gone, and the client
// learns that as a lookup failure rather than a hang.
func TestRestartedServerForgetsSessions(t *testing.T) {
	// First deployment.
	clientA, _ := remoteWithTransport(t, newFlaky)
	old, err := clientA.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	oldID := old.ID()

	// Second deployment over a fresh runner, as a restart would be.
	clientB, _ := remoteWithTransport(t, newFlaky)

	list, err := clientB.List(context.Background())
	if err != nil {
		t.Fatalf("List on the new server: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a fresh server lists %d sessions", len(list))
	}
	if err := clientB.Terminate(context.Background(), oldID); !errdefs.IsNotFound(err) {
		t.Fatalf("Terminate of a pre-restart id = %v, want NotFound", err)
	}
}

// When the server is gone, Close still marks the handle closed: the client owns
// its own state, and a second Close must not retry the failed drain.
func TestCloseWithTheServerGone(t *testing.T) {
	root := t.TempDir()
	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithServerToken(testToken))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	listener := httptest.NewServer(server)

	client, err := remote.New(context.Background(), listener.URL, remote.WithToken(testToken))
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer func() { _ = client.Close() }()

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"sh", "-c", "sleep 5"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The server disappears underneath the client.
	_ = server.Close()
	listener.Close()

	if err := client.Close(); err == nil {
		t.Log("Close reported no error; the drain is best effort and may have raced the shutdown")
	}
	if _, err := sess.Read(context.Background(), 0, 16); !errors.Is(err, sandbox.ErrSessionClosed) {
		t.Fatalf("Read after a failed Close = %v, want ErrSessionClosed", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// pipeTransport serves a scripted session: a start that always succeeds, one
// stream of frames, and nothing else. It is how a slow consumer is exercised
// without a real server holding a real process.
type pipeTransport struct {
	t      *testing.T
	frames int
}

func (p *pipeTransport) Do(_ context.Context, op remote.Op, _ []byte, res any) error {
	switch op {
	case remote.OpHello:
		*(res.(*wire.Hello)) = wire.Hello{
			Version:      wire.Version,
			Capabilities: sandbox.Capabilities{Features: sandbox.SessionFeatures{Events: true}},
		}
	case remote.OpStart:
		*(res.(*wire.StartResponse)) = wire.StartResponse{
			ID:           "pipe",
			Capabilities: sandbox.SessionCapabilities{Events: true},
		}
	case remote.OpClose:
		*(res.(*wire.CloseResponse)) = wire.CloseResponse{}
	default:
		return errdefs.NotAvailablef("pipeTransport: %s", op)
	}
	return nil
}

func (p *pipeTransport) DoBytes(context.Context, remote.Op, []byte) ([]byte, error) {
	return nil, errdefs.NotAvailablef("pipeTransport: no byte answers")
}

func (p *pipeTransport) Stream(ctx context.Context, op remote.Op, _ []byte) (io.ReadCloser, error) {
	if op != remote.OpEvents {
		return nil, errdefs.NotAvailablef("pipeTransport: %s", op)
	}
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		for i := range p.frames {
			frame := wire.Event{Seq: int64(i), Type: wire.EventOutput, Data: []byte("x")}
			data, err := wire.Encode(frame)
			if err != nil {
				return
			}
			// A synchronous pipe: this blocks until the client's pump reads,
			// which is exactly the backpressure a slow consumer creates.
			if _, err := fmt.Fprintf(writer, "id: %d\nevent: output\ndata: %s\n\n", i, data); err != nil {
				return
			}
		}
		frame := wire.Event{Seq: int64(p.frames), Type: wire.EventExited, Exit: &sandbox.SessionExit{}}
		data, _ := wire.Encode(frame)
		_, _ = fmt.Fprintf(writer, "id: %d\nevent: exited\ndata: %s\n\n", p.frames, data)
	}()
	return reader, nil
}

func concat(chunks []sandbox.OutputChunk) string {
	var out []byte
	for _, c := range chunks {
		out = append(out, c.Data...)
	}
	return string(out)
}
