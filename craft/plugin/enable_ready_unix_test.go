//go:build !windows

// The failure this file pins is a POSIX one: a file the manifest check
// accepts and exec refuses because it has no execute bit. Windows refuses
// such a file for a different reason, so the assertion on the errno below
// is not portable.

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestHostEnableReportsUnrunnableCommand covers the window the install
// check cannot close: the command was runnable when the plugin was
// installed and is not any more. The enable reports it instead of
// recording a plugin whose process never arrives.
func TestHostEnableReportsUnrunnableCommand(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", wireManifest("hello", "tools.provide"))
	// A regular file the manifest check accepts — it only asks for a
	// regular file — and exec cannot run.
	unrunnable := filepath.Join(root, "hello", "server")
	if err := os.WriteFile(unrunnable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write server: %v", err)
	}
	// A bound no test would reach by waiting: the failure is terminal, so
	// the enable has to return long before it.
	plugin := realPluginHost(t, root, HostOptions{EnableTimeout: time.Minute})
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)
	hook := &hookRecorder{}
	plugin.host.SetPluginHooks(nil, hook.record)

	start := time.Now()
	err := plugin.host.SetEnabled(context.Background(), "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("enable = %v, want a terminal failure", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("enable error = %v, want the spawn failure named", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("a terminal failure took %s: the wait ran the bound out", elapsed)
	}
	if enabled(t, plugin.store, "hello") {
		t.Fatal("the failed enable was recorded as enabled")
	}
	if running(plugin.host, "hello") {
		t.Fatal("the source of a failed enable is still recorded")
	}
	if got := toolNames(plugin.host); len(got) != 0 {
		t.Fatalf("tools = %v, want none", got)
	}
	// An open that failed never started a process, so there is no stop
	// to report: the enable's error is the report. What the enable did
	// mint is still revoked — the environment hook ran before the
	// spawn — and that is the internal hook, not a subscriber event.
	if got := events.snapshot(); len(got) != 0 {
		t.Fatalf("stop events = %v, want none for a process that never started", got)
	}
	if got := hook.count(); got != 1 {
		t.Fatalf("the internal stopped hook ran %d times, want 1", got)
	}
}
