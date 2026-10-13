//go:build !windows

// The failure this file pins is a POSIX one: a file the manifest
// check accepts and exec refuses because it has no execute bit.
// Windows refuses such a file for a different reason — and with a
// different errno, which connectError does not classify as
// terminal — so there the enable would wait out its bound instead
// of reporting the failure at once.

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	writePlugin(t, root, "hello", helperManifest())
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

	start := time.Now()
	err := plugin.host.SetEnabled(context.Background(), "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("enable = %v, want a terminal failure", err)
	}
	if !strings.Contains(err.Error(), "cannot start command") {
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
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:rolled_back"}) {
		t.Fatalf("stop events = %v, want [hello:rolled_back]", got)
	}
}

// TestHostEnableReportsTheFirstTerminalServer covers a plugin whose
// servers fail differently: the terminal answer of the first one is the
// answer, and the servers after it — one of which would have waited out
// the whole bound — do not change it.
func TestHostEnableReportsTheFirstTerminalServer(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", multiServerManifest(unrunnableServer(), unreachableServer()))
	// The command is a regular file the manifest check accepts and exec
	// cannot run.
	if err := os.WriteFile(filepath.Join(root, "hello", "server"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write server: %v", err)
	}
	// A bound no test would reach by waiting: the first server's failure
	// is terminal, so the enable answers with it long before the second
	// server's bound would have elapsed.
	plugin := realPluginHost(t, root, HostOptions{EnableTimeout: time.Minute})
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)

	start := time.Now()
	err := plugin.host.SetEnabled(context.Background(), "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("enable = %v, want the first server's terminal failure", err)
	}
	if !strings.Contains(err.Error(), "cannot start command") {
		t.Fatalf("enable error = %v, want the spawn failure named", err)
	}
	if !strings.Contains(err.Error(), `server "hello"`) {
		t.Fatalf("enable error = %v, want the failing server named", err)
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
	if got := events.snapshot(); !slices.Equal(got, []string{"hello:rolled_back"}) {
		t.Fatalf("stop events = %v, want [hello:rolled_back]", got)
	}
}

// TestHostEnableRetriesAfterATerminalLoss covers re-enabling the plugin
// the application was just told about. The process died, the command
// cannot be spawned any more, and the enable is an attempt that reports
// its failure — rather than a call that finds the dead source "already
// running" and returns nil for a plugin that has no process.
func TestHostEnableRetriesAfterATerminalLoss(t *testing.T) {
	plugin := startStopPluginHost(t, 0)
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)

	pids := plugin.waitForPIDs(t, 1, 15*time.Second)
	pid, err := strconv.Atoi(pids[0])
	if err != nil {
		t.Fatalf("helper pid %q: %v", pids[0], err)
	}
	// The command is there and is no longer executable — files change
	// under an installed plugin — so the reconnect after the kill cannot
	// spawn it again and gives up.
	server := filepath.Join(plugin.dir, "server")
	if err := os.Remove(server); err != nil {
		t.Fatalf("remove server: %v", err)
	}
	if err := os.WriteFile(server, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write server: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal helper: %v", err)
	}
	waitForStops(t, events, []string{"hello:" + string(StopFailed)})

	err = plugin.host.SetEnabled(context.Background(), "hello", true)
	if !errdefs.IsValidation(err) {
		t.Fatalf("enable after a loss = %v, want the spawn failure", err)
	}
	if !strings.Contains(err.Error(), "cannot start command") {
		t.Fatalf("enable error = %v, want the spawn failure named", err)
	}
	if got := events.snapshot(); !slices.Equal(got, []string{
		"hello:" + string(StopFailed), "hello:" + string(StopRolledBack),
	}) {
		t.Fatalf("stop events = %v, want the loss and the rollback", got)
	}
	if entry, ok := plugin.host.Store().Entry("hello"); !ok || entry.Enabled {
		t.Fatalf("entry = %+v (found %v), want the plugin recorded disabled", entry, ok)
	}
	if running(plugin.host, "hello") {
		t.Fatal("the source of the failed retry is still recorded")
	}
	if got := toolNames(plugin.host); len(got) != 0 {
		t.Fatalf("tools = %v, want none", got)
	}
}

// unrunnableServer is one stdio server whose command is a regular file
// without an execute bit: the manifest check accepts it and exec refuses
// it.
func unrunnableServer() string {
	return `{"command": "./server"}`
}

// waitForStops blocks until the recorded stops are exactly want.
func waitForStops(t *testing.T, events *stopEvents, want []string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if got := events.snapshot(); len(got) >= len(want) {
			if !slices.Equal(got, want) {
				t.Fatalf("stop events = %v, want %v", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stop events = %v, want %v", events.snapshot(), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHostEnableReportsLossWhileWaitingOnAnotherServer covers the
// interleaving a plugin with several servers reaches in practice: the
// server whose turn has already passed gives up on its own while the
// enable is still waiting on a later one, and the call then ends without
// a verdict — the later server is merely not there yet. The loss the wait
// collected is one the application has to hear, or the entry says enabled
// about a plugin one of whose servers has no process and no report.
func TestHostEnableReportsLossWhileWaitingOnAnotherServer(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "hello", multiServerManifest(helperServer(), unreachableServer()))
	writeHelperCommand(t, filepath.Join(root, "hello"))
	// A bound far longer than the orchestration below, and short enough
	// that the test waits it out rather than the reverse.
	bound := 2 * time.Second
	plugin := realPluginHost(t, root, HostOptions{EnableTimeout: bound})
	events := &stopEvents{}
	plugin.host.OnStopped(events.record)

	enableDone := make(chan error, 1)
	go func() {
		enableDone <- plugin.host.SetEnabled(context.Background(), "hello", true)
	}()

	// The helper is serving — its tools are published, so the wait has
	// moved on to the remote server that is down — and its process dies
	// with no way back: the command is there and is no longer executable.
	waitForToolUpTo(t, plugin.host, "hello__1__echo", 20*time.Second)
	pid := helperPID(t, plugin, 20*time.Second)
	server := filepath.Join(root, "hello", "server")
	if err := os.Remove(server); err != nil {
		t.Fatalf("remove server: %v", err)
	}
	if err := os.WriteFile(server, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write server: %v", err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal helper: %v", err)
	}

	select {
	case err := <-enableDone:
		if err != nil {
			t.Fatalf("enable of a plugin whose other server is down: %v", err)
		}
	case <-time.After(bound + 20*time.Second):
		t.Fatal("the enable never returned")
	}
	waitForStops(t, events, []string{"hello:" + string(StopFailed)})
	if !enabled(t, plugin.store, "hello") {
		t.Fatal("a server that is merely not up yet rolled the enable back")
	}
	if !running(plugin.host, "hello") {
		t.Fatal("the source was dropped instead of left reconnecting")
	}
}

// helperPID blocks until the plugin's child recorded its pid, which the
// stop helper does on startup. The plugin's servers are declared in
// order, so the first pid is the first server's process.
func helperPID(t *testing.T, plugin *realPlugin, timeout time.Duration) int {
	t.Helper()
	path := filepath.Join(plugin.dataRoot, "hello", "pids")
	deadline := time.Now().Add(timeout)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(raw))
			if len(fields) > 0 {
				pid, convErr := strconv.Atoi(fields[0])
				if convErr != nil {
					t.Fatalf("helper pid %q: %v", fields[0], convErr)
				}
				return pid
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read pid file: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the plugin's helper never recorded its pid")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
