//go:build windows

package mcp

import (
	"context"
	"testing"
	"time"
)

// TestWindowsStdioExitsOnStdinClose verifies the graceful path: a
// child that exits when its stdin closes is reaped on Close without
// waiting out any grace period or killing anything. The MCP spec's
// "close stdin and wait for a graceful exit" step must run first.
func TestWindowsStdioExitsOnStdinClose(t *testing.T) {
	tport, err := Stdio("cmd", []string{"/c", "more"}, nil)
	if err != nil {
		t.Fatalf("Stdio: %v", err)
	}
	conn, err := tport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	start := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v, want prompt reaping after stdin close", elapsed)
	}
	// Close is idempotent and must not panic or return a different
	// outcome on a second call.
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestWindowsStdioKillFallback verifies the escalation tail: a child
// that ignores stdin and does not react to CTRL+BREAK is killed after
// the bounded graces instead of hanging Close forever.
func TestWindowsStdioKillFallback(t *testing.T) {
	old := defaultMCPShutdownGrace
	defaultMCPShutdownGrace = 200 * time.Millisecond
	t.Cleanup(func() { defaultMCPShutdownGrace = old })

	tport, err := Stdio("cmd", []string{"/c", "ping", "-n", "30", "127.0.0.1", ">nul"}, nil)
	if err != nil {
		t.Fatalf("Stdio: %v", err)
	}
	conn, err := tport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	start := time.Now()
	if err := conn.Close(); err == nil {
		t.Fatal("Close succeeded, want the kill-fallback error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close took %v, want bounded escalation (grace+grace+kill)", elapsed)
	}
}
