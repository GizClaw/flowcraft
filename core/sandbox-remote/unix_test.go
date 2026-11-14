//go:build unix

package remote_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// The unix socket transport, end to end: the same protocol over a socket whose
// file permissions are the boundary instead of a shared secret.
//
// The conformance suite is the assertion, because "the transport works" means
// exactly "the contract survives this transport" — the same thing it means for
// HTTP.
func TestConformanceOverUnixSocket(t *testing.T) {
	sandboxtest.Run(t, func(t *testing.T, root string) sandboxtest.Harness {
		t.Helper()
		return sandboxtest.Harness{
			Runner: newUnixRunner(t, root),
			Root:   root,
		}
	})
}

// newUnixRunner wires local -> unix socket -> client and returns the client side.
//
// On unix the socket's mode is the credential, so no token is needed. On Windows
// os.Chmod is advisory and access is decided by the directory ACL, so the token
// stays: the transport is identical, the authentication is not.
func newUnixRunner(t *testing.T, root string) *remote.Runner {
	t.Helper()

	inner := local.New(root)
	t.Cleanup(func() { _ = inner.Close() })

	// A unix socket is the boundary: no token, as on a host that trusts the
	// directory's permissions.
	serverOpts := []remote.ServerOption{remote.WithoutAuth()}
	var clientOpts []remote.Option

	server, err := remote.NewServer(inner, serverOpts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// A short directory: sockaddr_un bounds the whole path, and t.TempDir() is
	// long enough on some platforms to approach that limit.
	dir, err := os.MkdirTemp("", "sbx-unix")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "s.sock")

	listener, err := remote.ListenUnix(socketPath)
	if err != nil {
		t.Skipf("unix sockets are not available here: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := remote.NewUnix(ctx, socketPath, clientOpts...)
	if err != nil {
		t.Fatalf("NewUnix: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// The socket file is created with the mode that makes it a credential.
func TestListenUnixCreatesAPrivateSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "sbx-unix")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// The parent directory does not exist yet: ListenUnix creates it 0700.
	socketPath := filepath.Join(dir, "nested", "s.sock")
	listener, err := remote.ListenUnix(socketPath)
	if err != nil {
		t.Skipf("unix sockets are not available here: %v", err)
	}
	defer func() { _ = listener.Close() }()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(socketPath))
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket directory mode = %o, want group/other access off", perm)
	}
}

// A path that cannot fit in sockaddr_un is refused with the reason, instead of
// surfacing as a generic listen failure.
func TestUnixPathLengthIsChecked(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("a", 200), "s.sock")

	if _, err := remote.NewUnixTransport(long); !errdefs.IsValidation(err) {
		t.Fatalf("NewUnixTransport(long path) = %v, want Validation", err)
	}
	if _, err := remote.ListenUnix(long); !errdefs.IsValidation(err) {
		t.Fatalf("ListenUnix(long path) = %v, want Validation", err)
	}
	if _, err := remote.NewUnixTransport(""); !errdefs.IsValidation(err) {
		t.Fatalf("NewUnixTransport(empty) = %v, want Validation", err)
	}
}

// A socket left by a crashed server is reported, not silently unlinked: the path
// may belong to a live process.
func TestListenUnixRefusesAStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "sbx-unix")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	socketPath := filepath.Join(dir, "s.sock")

	first, err := remote.ListenUnix(socketPath)
	if err != nil {
		t.Skipf("unix sockets are not available here: %v", err)
	}
	defer func() { _ = first.Close() }()

	if _, err := remote.ListenUnix(socketPath); err == nil {
		t.Fatal("a second listen on the same path succeeded; the first socket was replaced")
	}
}

// Dialing a socket nobody listens on is a transport failure, not a protocol one,
// so a caller can tell "the server is gone" from "the server refused me".
func TestNewUnixWithoutAServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "sbx-unix")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = remote.NewUnix(ctx, filepath.Join(dir, "absent.sock"))
	if err == nil {
		t.Fatal("NewUnix connected to a socket nobody listens on")
	}
	var transportErr *remote.TransportError
	if !errors.As(err, &transportErr) {
		t.Fatalf("err = %v, want a transport failure", err)
	}
}
