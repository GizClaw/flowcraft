//go:build linux

// Package bridge implements the in-netns half of bwrap NetAllowList /
// NetProxy enforcement. It is not a standalone binary: the host
// application embeds it and calls [MaybeRun] at the very top of main,
// and the bwrap runner re-executes the host binary with a reserved
// first argument so this package can take over as the sandboxed
// command. This mirrors how Codex dispatches on argv[0] to turn one
// binary into both the CLI and the sandbox helper.
//
// When invoked as the bridge, the process listens on the netns
// loopback, forwards every accepted connection over a unix socket to
// the host-side enforcement proxy (core/utils/net,
// whose socket path is bind-mounted into the sandbox), injects the
// proxy environment (HTTP(S)_PROXY / ALL_PROXY -> 127.0.0.1:<port>,
// NO_PROXY stripped) so proxy-aware clients use the single enforced
// egress, then runs the real command as a child and propagates its
// exit status.
package bridge

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Marker is the reserved argv[1] that selects bridge mode when the
// bwrap runner re-executes the host binary. It is deliberately
// namespaced so it cannot collide with a normal CLI flag.
const Marker = "--flowcraft-bwrap-bridge"

// MaybeRun reports whether the current process was re-executed as the
// in-netns bridge and, if so, runs it to completion, exiting with the
// proxied command's exit status (128+signal when signalled). It
// returns false for every normal invocation so the host application
// can simply do:
//
//	if bridge.MaybeRun() {
//		return
//	}
//
// at the top of main, before its own flag parsing. The call is safe
// on every platform; only the re-exec'd Linux process matches Marker.
func MaybeRun() bool {
	if !isBridgeInvocation(os.Args) {
		return false
	}
	code := run(os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
	return true // unreachable
}

// isBridgeInvocation reports whether argv selects bridge mode: argv[0]
// is the host binary and argv[1] is the reserved marker. The bwrap
// runner always re-executes the host binary with exactly this shape.
func isBridgeInvocation(argv []string) bool {
	return len(argv) >= 2 && argv[1] == Marker
}

// run parses "--sock <path> -- <cmd> [args...]" (the argv after
// Marker), serves the bridge until the child exits, and returns the
// child's exit code. It is split out of MaybeRun so tests can drive
// the bridge logic without a subprocess.
func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sock := fs.String("sock", "", "host proxy unix socket path (bind-mounted into the sandbox)")
	if err := fs.Parse(argv); err != nil {
		_, _ = fmt.Fprintf(stderr, "bridge: parse: %v\n", err)
		return 2
	}
	args := fs.Args()
	if *sock == "" || len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: --sock <path> -- <cmd> [args...]")
		return 2
	}

	// Listen before starting the child so the proxy port is ready when
	// the command first connects. The ephemeral port avoids needing
	// CAP_NET_BIND_SERVICE.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bridge: listen: %v\n", err)
		return 1
	}
	port := ln.Addr().(*net.TCPAddr).Port

	c := exec.Command(args[0], args[1:]...)
	c.Stdin = stdin
	c.Stdout = stdout
	c.Stderr = stderr
	c.Env = childEnv(port)
	if err := c.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "bridge: start %s: %v\n", args[0], err)
		return 127
	}

	go acceptLoop(ln, *sock, stderr)

	// Propagate the command's exit status (bash-compatible 128+signal).
	if err := c.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code := ee.ExitCode()
			if code < 0 {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					code = 128 + int(ws.Signal())
				} else {
					code = 1
				}
			}
			return code
		}
		_, _ = fmt.Fprintf(stderr, "bridge: wait: %v\n", err)
		return 127
	}
	return 0
}

// childEnv builds the command's environment: the bridge process env
// (already shaped by bwrap's --clearenv / --setenv) with every proxy
// variable pointed at the bridge's loopback port and NO_PROXY removed
// so clients cannot opt out into a connection the enforcement proxy
// would deny anyway. Building the env explicitly (instead of mutating
// the bridge's own env) keeps the logic testable in-process.
func childEnv(port int) []string {
	proxy := fmt.Sprintf("http://127.0.0.1:%d", port)
	var env []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HTTP_PROXY="+proxy, "http_proxy="+proxy,
		"HTTPS_PROXY="+proxy, "https_proxy="+proxy,
		"ALL_PROXY="+proxy, "all_proxy="+proxy,
		"NO_PROXY=", "no_proxy=",
	)
	return env
}

// acceptLoop forwards each accepted loopback connection over a fresh
// unix-socket connection to the host proxy.
func acceptLoop(ln net.Listener, sock string, stderr io.Writer) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go forward(conn, sock, stderr)
	}
}

// forward pipes one TCP connection to one UDS connection (the host
// proxy treats each UDS connection as one client connection).
func forward(tcp net.Conn, sock string, stderr io.Writer) {
	defer func() {
		if err := tcp.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "bridge: close tcp: %v\n", err)
		}
	}()
	uds, err := net.Dial("unix", sock)
	if err != nil {
		return
	}
	defer func() {
		if err := uds.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "bridge: close uds: %v\n", err)
		}
	}()
	go func() {
		_, _ = io.Copy(uds, tcp)
		if err := uds.Close(); err != nil {
			_, _ = fmt.Fprintf(stderr, "bridge: close uds after copy: %v\n", err)
		}
	}()
	_, _ = io.Copy(tcp, uds)
}
