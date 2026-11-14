package remote

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/httpkit"
)

// Unix-domain sockets are the same-host transport: no listening port, no network
// stack, and — on unix — the socket file's permissions as the credential. The
// protocol is unchanged; only where the bytes travel is.
//
// # Platform notes
//
// AF_UNIX exists on Linux and macOS, and on Windows since 10.0.17063 (Go probes it
// at runtime with WSASocket and fails the listen otherwise). Two differences
// matter here:
//
//   - Path length is bounded by the platform's sockaddr_un: 108 bytes on Windows
//     (UNIX_PATH_MAX in afunix.h, where C:\Users\...\Temp\... is easily longer) and
//     104 on macOS. A path that does not fit fails at listen time with a confusing
//     error, so this package checks it first.
//   - File permissions are a real credential only on unix. On Windows os.Chmod
//     maps to the read-only attribute and access is decided by the directory ACL,
//     which is why a Windows deployment should keep the bearer token even on a
//     socket.
const maxUnixPathBytes = 100

// NewUnixTransport returns a Transport that speaks the protocol over the unix
// domain socket at path.
//
// The base URL of the underlying HTTP transport is a placeholder: the dialer
// decides where the connection goes, so the host in each request URL is never
// resolved. Token, timeout and TLS options apply as they do over TCP (TLS is
// meaningless on a socket; the kernel carries the bytes).
func NewUnixTransport(path string, opts ...HTTPOption) (Transport, error) {
	if err := checkUnixPath(path); err != nil {
		return nil, err
	}
	all := append([]HTTPOption{WithHTTPClient(unixHTTPClient(path))}, opts...)
	return NewHTTPTransport(unixPlaceholderBase, all...), nil
}

// NewUnix connects to a server listening on the unix domain socket at path.
//
// It is [New] with the transport chosen for you: the options are the same, and a
// token is only meaningful where the socket's permissions are not the boundary
// (see the package notes on Windows).
func NewUnix(ctx context.Context, path string, opts ...Option) (*Runner, error) {
	cfg := newClientOptions(opts)
	transport, err := NewUnixTransport(path, cfg.httpOptions()...)
	if err != nil {
		return nil, err
	}
	cfg.transport = transport
	return newRunner(ctx, cfg)
}

// ListenUnix prepares a listener for [Server] on the unix domain socket at path.
//
// It creates the parent directory (0700) if it does not exist and sets the socket
// mode to 0600: on unix that file is the credential, which is why a deployment can
// then use [WithoutAuth] instead of carrying a token. On Windows the mode is
// advisory and the directory ACL decides, so keep the token there.
//
// A socket left behind by a crashed process makes the listen fail rather than
// being unlinked: removing a path that a live server may own would hijack its
// traffic. An operator (or a supervisor that owns the directory) clears it.
func ListenUnix(path string) (net.Listener, error) {
	if err := checkUnixPath(path); err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, errdefs.Internalf("sandbox-remote: create socket directory: %w", err)
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errdefs.NotAvailablef(
			"sandbox-remote: listen on unix socket %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		if cerr := listener.Close(); cerr != nil {
			return nil, errdefs.Internalf(
				"sandbox-remote: close socket after chmod failure: %w", cerr)
		}
		return nil, errdefs.Internalf("sandbox-remote: chmod socket %q: %w", path, err)
	}
	return listener, nil
}

// unixPlaceholderBase is a URL the transport never resolves: the dialer ignores
// the host, and the path is all the HTTP layer needs to build a request.
const unixPlaceholderBase = "http://sandbox-unix.invalid"

// unixHTTPClient builds the client whose dial goes to the socket.
func unixHTTPClient(path string) *http.Client {
	client := httpkit.NewHTTPClient(httpkit.WithoutRetry())
	client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		},
		// The event stream holds one connection open per watcher; keep a few
		// around so a reconnect does not have to wait for the listener.
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
	}
	return client
}

// checkUnixPath rejects a path that cannot fit in sockaddr_un before the kernel
// reports it as a generic listen failure.
func checkUnixPath(path string) error {
	if path == "" {
		return errdefs.Validationf("sandbox-remote: empty unix socket path")
	}
	if len(path) > maxUnixPathBytes {
		return errdefs.Validationf(
			"sandbox-remote: unix socket path is %d bytes, the platform limit is %d "+
				"(sockaddr_un; use a shorter directory)", len(path), maxUnixPathBytes)
	}
	return nil
}
