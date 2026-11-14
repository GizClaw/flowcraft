//go:build unix

// Sessions run on the unix spawn path. The Windows backend has its own
// conformance file, and these bodies are POSIX scripts: making this suite run
// there means rendering the bodies (not just the shell) through
// sandboxtest.ForPlatform, which lands with the sessionkit port.

package remote_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// serve builds a local-backed server and returns its URL and a client pointed at
// it with the given options.
func serve(t *testing.T, serverOpts []remote.ServerOption, clientOpts ...remote.Option) (*remote.Runner, string) {
	t.Helper()
	inner := local.New(t.TempDir())
	t.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, serverOpts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := remote.New(ctx, listener.URL, clientOpts...)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, listener.URL
}

// An endpoint that executes commands must not come up unauthenticated by
// omission: the decision has to be stated.
func TestNewServerRequiresAnAuthDecision(t *testing.T) {
	inner := local.New(t.TempDir())
	defer func() { _ = inner.Close() }()

	if _, err := remote.NewServer(inner); !errdefs.IsValidation(err) {
		t.Fatalf("NewServer without auth = %v, want Validation", err)
	}
	if _, err := remote.NewServer(inner, remote.WithoutAuth()); err != nil {
		t.Fatalf("NewServer(WithoutAuth) = %v", err)
	}
	if _, err := remote.NewServer(nil, remote.WithoutAuth()); !errdefs.IsValidation(err) {
		t.Fatalf("NewServer(nil) = %v, want Validation", err)
	}
}

func TestUnauthenticatedClientIsRefused(t *testing.T) {
	inner := local.New(t.TempDir())
	defer func() { _ = inner.Close() }()
	server, err := remote.NewServer(inner, remote.WithServerToken(testToken))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = server.Close() }()
	listener := httptest.NewServer(server)
	defer listener.Close()

	for name, opts := range map[string][]remote.Option{
		"no token":    nil,
		"wrong token": {remote.WithToken("not-the-token")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := remote.New(context.Background(), listener.URL, opts...)
			// The handshake is the first request, so an auth failure surfaces
			// here rather than on the first command.
			if !errdefs.IsUnauthorized(err) {
				t.Fatalf("remote.New = %v, want Unauthorized", err)
			}
		})
	}
}

func TestWithoutAuthServes(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithoutAuth()})
	if _, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo hi"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// The id space belongs to the server: asking about one it never minted is a
// lookup failure, not a capability failure.
func TestUnknownSessionIsNotFound(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	if err := client.Terminate(context.Background(), "never-existed"); !errdefs.IsNotFound(err) {
		t.Fatalf("Terminate = %v, want NotFound", err)
	}
	// Re-attach is the server backend's capability, not the wire's: a
	// sandbox-local server can hand a handle back (its registry still holds the
	// process), so an id it never minted is a lookup failure.
	if _, err := sandbox.Attach(context.Background(), client, "never-existed"); !errdefs.IsNotFound(err) {
		t.Fatalf("Attach = %v, want NotFound", err)
	}
}

// A journal the server does not have is NotAvailable on the client, which is the
// contract's answer — not an empty stream that looks like "nothing was written".
func TestJournalWithoutOneIsNotAvailable(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	if _, err := sandbox.OpenJournal(context.Background(), client); !errdefs.IsNotAvailable(err) {
		t.Fatalf("OpenJournal = %v, want NotAvailable", err)
	}
}

// Capabilities travel verbatim: a client that edited them would turn "ask before
// trying" into a failed call. Every dimension is compared, because a narrowing
// disguised as a proxy decision is exactly the kind of drift this test exists to
// catch.
func TestCapabilitiesAreTheServers(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	inner := local.New(t.TempDir())
	defer func() { _ = inner.Close() }()
	want := inner.Capabilities()

	if got := client.Capabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %+v, want the server's %+v", got, want)
	}
}

// A closed handle refuses everything but Wait, which answers from the exit its
// Close reaped.
func TestClosedSessionRefusesOperations(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Read to EOF first: that is the signal the process produced its exit status.
	// Closing a live process would legitimately reap it as terminated (-1), and
	// the assertion below is about the exit Close reports, not about the race.
	drainToEOF(t, sess)
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := sess.Read(context.Background(), 0, 16); !errors.Is(err, sandbox.ErrSessionClosed) {
		t.Errorf("Read after Close = %v, want ErrSessionClosed", err)
	}
	if err := sess.Write(context.Background(), []byte("x")); !errors.Is(err, sandbox.ErrSessionClosed) {
		t.Errorf("Write after Close = %v, want ErrSessionClosed", err)
	}
	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait after Close = %v, want the reaped exit", err)
	}
	if exit.Code != 7 {
		t.Fatalf("exit code = %d, want 7", exit.Code)
	}
}

// A policy the server's backend cannot enforce is refused by the server, and the
// client's caller sees the same classification it would see locally.
func TestUnenforceablePolicySurvivesTheWire(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	_, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "true"},
		Opts: sandbox.ExecOptions{
			Resources: sandbox.ResourceLimits{DiskBytes: 1 << 20},
		},
	})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start with DiskBytes = %v, want NotAvailable", err)
	}
}

// A wire version this client does not speak is refused at the handshake instead
// of failing somewhere in the middle of a session.
func TestVersionMismatchIsRefused(t *testing.T) {
	_, err := remote.New(context.Background(), "http://unused",
		remote.WithTransport(helloOnly{version: wire.Version + 1}))
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("New = %v, want NotAvailable", err)
	}
	if err != nil && !strings.Contains(err.Error(), "wire version") {
		t.Fatalf("error = %v, want it to name the wire version", err)
	}
}

// helloOnly answers the handshake and nothing else.
type helloOnly struct{ version int }

func (h helloOnly) DoBytes(context.Context, remote.Op, []byte) ([]byte, error) {
	return nil, errdefs.NotAvailablef("helloOnly: no byte answers")
}

func (h helloOnly) Stream(context.Context, remote.Op, []byte) (io.ReadCloser, error) {
	return nil, errdefs.NotAvailablef("helloOnly: no streams")
}

func (h helloOnly) Do(_ context.Context, op remote.Op, _ []byte, res any) error {
	if op != remote.OpHello {
		return errdefs.NotAvailablef("helloOnly: only the handshake is supported")
	}
	hello, ok := res.(*wire.Hello)
	if !ok {
		return errdefs.Internalf("helloOnly: unexpected response type %T", res)
	}
	*hello = wire.Hello{
		Version:      h.version,
		Server:       "fake",
		Capabilities: sandbox.Capabilities{Policy: sandbox.Enforcement{NetModes: permission.IsolatingNetModes()}},
	}
	return nil
}

// drainToEOF reads until the process exited, which is what makes a following
// assertion about its exit status deterministic.
func drainToEOF(t *testing.T, sess sandbox.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	after := int64(0)
	for {
		out, err := sess.Read(ctx, after, 4096)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		after = out.NextSeq
		if out.EOF {
			return
		}
	}
}

// Attach over the wire: the server backend holds the process, so an id is enough
// to read the output log again — which is what a reconnecting client needs.
func TestAttachToAStartedSession(t *testing.T) {
	client, _ := serve(t, []remote.ServerOption{remote.WithServerToken(testToken)},
		remote.WithToken(testToken))

	sess, err := client.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo reattached"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let it finish so the log is complete before anyone re-attaches.
	drainToEOF(t, sess)

	attached, err := sandbox.Attach(context.Background(), client, sess.ID())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer func() { _ = attached.Close() }()

	if attached.ID() != sess.ID() || attached.PID() != sess.PID() {
		t.Fatalf("attached = %q/%d, want %q/%d",
			attached.ID(), attached.PID(), sess.ID(), sess.PID())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []byte
	after := int64(0)
	for {
		chunk, err := attached.Read(ctx, after, 4096)
		if err != nil {
			t.Fatalf("Read through the attached handle: %v", err)
		}
		for _, c := range chunk.Chunks {
			out = append(out, c.Data...)
		}
		after = chunk.NextSeq
		if chunk.EOF {
			break
		}
	}
	if !strings.Contains(string(out), "reattached") {
		t.Fatalf("replayed output = %q, want the session's line", out)
	}
	if exit, err := attached.Wait(ctx); err != nil || exit.Code != 0 {
		t.Fatalf("Wait = %+v, %v", exit, err)
	}
}
