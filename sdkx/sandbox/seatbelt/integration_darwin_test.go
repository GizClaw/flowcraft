//go:build integration_seatbelt && darwin

package seatbelt

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// TestIntegration_NetDenyAll proves the generated SBPL profile blocks
// even loopback connections at the OS level. A local listener avoids
// coupling the test to internet or DNS availability.
func TestIntegration_NetDenyAll(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port

	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/usr/bin/nc",
		[]string{"-z", "-w", "1", "127.0.0.1", strconv.Itoa(port)},
		sandbox.ExecOptions{
			Timeout: 3 * time.Second,
			Net:     sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("NetDenyAll allowed loopback connection to %s: %+v",
			fmt.Sprintf("127.0.0.1:%d", port), result)
	}
}
