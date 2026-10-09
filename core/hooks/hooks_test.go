package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// requirePosixShell skips a test that runs a real hook. Commands run
// under sh -c with POSIX conventions — relative commands, shell
// redirection, a process group a timeout can kill — and the windows
// lane has none of those, so there the package is built and vetted
// rather than executed. That still compiles exec_windows.go, which is
// the half only windows can check.
func requirePosixShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hook execution needs a POSIX shell")
	}
}

// writeHooks writes a hooks.json into a fresh directory and returns its
// path.
func writeHooks(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}
	return path
}

// appendHook is a command that appends its stdin to path, so a test can
// count how often a hook ran and read what it received.
func appendHook(path string) string {
	return "cat >> " + path
}

// waitForContent waits until path holds non-blank content.
func waitForContent(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// The hook writes with a shell redirection, so the file can
		// exist before its content lands: wait for content, not for
		// existence.
		if data, err := os.ReadFile(path); err == nil &&
			len(strings.TrimSpace(string(data))) > 0 {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never received content", path)
	return nil
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	t.Parallel()
	manager, err := Load(context.Background(), filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !manager.Empty() {
		t.Fatal("missing file must yield an empty manager")
	}
	if manager.Path() == "" {
		t.Fatal("Path is empty")
	}
}

func TestLoadRejectsBadMatcher(t *testing.T) {
	t.Parallel()
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"matcher": "(", "hooks": [{"command": "true"}]}]
		}
	}`)
	if _, err := Load(context.Background(), path); err == nil {
		t.Fatal("bad matcher must be rejected")
	}
}

func TestLoadRejectsUnknownEvent(t *testing.T) {
	t.Parallel()
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "true"}]}],
			"PreToolUseX": [{"hooks": [{"command": "true"}]}]
		}
	}`)
	if _, err := Load(context.Background(), path); err == nil {
		t.Fatal("an event outside the vocabulary must be rejected, not ignored")
	}
}

func TestLoadRejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	path := writeHooks(t, `{
		"hooks": {"PreToolUse": [{"hooks": [{"command": "  "}]}]}
	}`)
	if _, err := Load(context.Background(), path); err == nil {
		t.Fatal("a hook without a command must be rejected")
	}
}

func TestLoadSkipsNonCommandHookTypes(t *testing.T) {
	t.Parallel()
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "*", "hooks": [
					{"type": "mcp_tool", "server": "x", "tool": "y"},
					{"type": "command", "command": "true"}
				]}
			]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if manager.Empty() {
		t.Fatal("the command hook should survive the kind filter")
	}
	groups := manager.groups[EventPreToolUse]
	if len(groups) != 1 || len(groups[0].hooks) != 1 {
		t.Fatalf("groups = %+v, want only the command hook", groups)
	}
}

func TestLoadDropsGroupsWithNoRunnableHooks(t *testing.T) {
	t.Parallel()
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [{"type": "mcp_tool", "server": "x"}]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !manager.Empty() {
		t.Fatal("a group with no command hook must not be kept")
	}
}

func TestFireRunsMatchingHookWithStdinPayload(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	out := filepath.Join(t.TempDir(), "hook.out")
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [
				{"matcher": "^exec_command$", "hooks": [
					{"command": "`+appendHook(out)+`", "timeout": 5}
				]}
			]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{
		"tool": "exec_command",
	})
	data := waitForContent(t, out)
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("hook stdin is not JSON: %v (%q)", err, data)
	}
	if payload["tool"] != "exec_command" {
		t.Fatalf("payload tool = %v", payload["tool"])
	}
	if payload["event"] != EventPreToolUse {
		t.Fatalf("payload event = %v, want the runner-stamped name", payload["event"])
	}

	// A non-matching tool and a different event run nothing: the same
	// output file keeps its size.
	before := len(data)
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "read_file"})
	manager.Fire(context.Background(), EventTurnEnd, map[string]any{"tool": "exec_command"})
	time.Sleep(50 * time.Millisecond)
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	if len(after) != before {
		t.Fatalf("hook ran for a non-matching occurrence: %d -> %d bytes", before, len(after))
	}
}

func TestFireAgainstEmptyManager(t *testing.T) {
	t.Parallel()
	manager, err := Load(context.Background(), filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Must not panic.
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
}

func TestFireMatcherUsesSourceField(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	out := filepath.Join(t.TempDir(), "hook.out")
	path := writeHooks(t, `{
		"hooks": {
			"SessionStart": [{"matcher": "^startup$", "hooks": [
				{"command": "`+appendHook(out)+`"}
			]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manager.Fire(context.Background(), EventSessionStart, map[string]any{"source": "startup"})
	waitForContent(t, out)

	other := filepath.Join(t.TempDir(), "other.out")
	path = writeHooks(t, `{
		"hooks": {
			"SessionStart": [{"matcher": "^resume$", "hooks": [
				{"command": "`+appendHook(other)+`"}
			]}]
		}
	}`)
	manager, err = Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manager.Fire(context.Background(), EventSessionStart, map[string]any{"source": "new"})
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("matcher must filter on the source field")
	}
}

func TestFireStampsEventNameAndLeavesPayloadAlone(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	out := filepath.Join(t.TempDir(), "hook.out")
	path := writeHooks(t, `{
		"hooks": {"TurnEnd": [{"hooks": [{"command": "`+appendHook(out)+`"}]}]}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	payload := map[string]any{"event": "PreToolUse"}
	manager.Fire(context.Background(), EventTurnEnd, payload)
	data := waitForContent(t, out)

	var received map[string]any
	if err := json.Unmarshal(data, &received); err != nil {
		t.Fatalf("hook stdin is not JSON: %v", err)
	}
	if received["event"] != EventTurnEnd {
		t.Fatalf("received event = %v, want %s", received["event"], EventTurnEnd)
	}
	if payload["event"] != "PreToolUse" {
		t.Fatalf("Fire mutated the producer's payload: %v", payload["event"])
	}
}

func TestFailingHookDoesNotBlockTheRest(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "second.out")
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [
				{"command": "exit 3"},
				{"command": "echo not-a-command-xyz"},
				{"command": "`+appendHook(out)+`"}
			]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
	// Fire returns no error and the hooks after the two failures ran.
	waitForContent(t, out)
}

func TestHookTimeoutKillsTheProcessGroup(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	late := filepath.Join(t.TempDir(), "late.out")
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [
				{"command": "(sleep 2; echo late > `+late+`) & sleep 30", "timeout": 1}
			]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	start := time.Now()
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("Fire took %s, want the 1s timeout to cut the hook short", elapsed)
	}
	// The hook spawned a descendant two levels down. Killing sh alone
	// would leave it running; the whole group has to go.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatal("a descendant of the timed-out hook survived the group kill")
	}
}

func TestHookThatLeavesABackgroundChildDoesNotStallFire(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	// The shell exits at once, but the background child keeps the
	// output pipe open: without a wait delay the runner would wait for
	// a process it does not own.
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "sleep 5 & exit 0"}]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	start := time.Now()
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Fatalf("Fire took %s, want the wait delay to bound it", elapsed)
	}
}

func TestCaptureTruncatesOutput(t *testing.T) {
	t.Parallel()
	limit := 32
	out := &capture{limit: limit}
	if _, err := out.Write([]byte(strings.Repeat("a", limit))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := out.String(); strings.Contains(got, "truncated") {
		t.Fatalf("output within the limit was marked truncated: %q", got)
	}
	if _, err := out.Write([]byte("bcd")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, strings.Repeat("a", limit)) {
		t.Fatalf("captured %q, want the first %d bytes", got, limit)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("output beyond the limit was not marked truncated: %q", got)
	}
	// The writer keeps accepting bytes: a full pipe would block the
	// hook.
	n, err := out.Write([]byte(strings.Repeat("c", 1<<16)))
	if err != nil || n != 1<<16 {
		t.Fatalf("Write after the limit = %d, %v; want every byte accepted", n, err)
	}
}

func TestHookOutputDoesNotBalloonTheRunner(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	// 1 MiB of output against a 64 KiB capture: Fire returns, and the
	// runner holds a bounded copy.
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "head -c 1048576 /dev/zero | tr '\\0' 'x'"}]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
}

func TestLoadWithSourcesAnchorsPluginCommands(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	pluginDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks.json"), []byte(`{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "cat > plugin-hook.out"}]}]
		}
	}`), 0o600); err != nil {
		t.Fatalf("write plugin hooks.json: %v", err)
	}
	manager, err := LoadWithSources(
		context.Background(),
		filepath.Join(t.TempDir(), "missing.json"),
		[]ExtraSource{{
			Path: filepath.Join(pluginDir, "hooks.json"),
			Dir:  pluginDir,
		}},
	)
	if err != nil {
		t.Fatalf("LoadWithSources: %v", err)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "exec_command"})
	waitForContent(t, filepath.Join(pluginDir, "plugin-hook.out"))
}

func TestLoadWithSourcesSkipsBrokenPluginFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write broken hooks.json: %v", err)
	}
	manager, err := LoadWithSources(
		context.Background(),
		filepath.Join(t.TempDir(), "missing.json"),
		[]ExtraSource{
			{Path: filepath.Join(t.TempDir(), "nope.json"), Dir: dir},
			{Path: filepath.Join(dir, "broken.json"), Dir: dir},
		},
	)
	if err != nil {
		t.Fatalf("a broken plugin source must be skipped, not fatal: %v", err)
	}
	if !manager.Empty() {
		t.Fatal("skipped sources must leave an empty manager")
	}
}

func TestBrokenPrimaryFileIsFatal(t *testing.T) {
	t.Parallel()
	path := writeHooks(t, "{")
	if _, err := Load(context.Background(), path); err == nil {
		t.Fatal("a broken primary hooks.json must fail the load")
	}
}

func TestUntrustedSourceSeesNoContentFields(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	pluginDir := t.TempDir()
	out := filepath.Join(pluginDir, "plugin-hook.out")
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks.json"), []byte(`{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "`+appendHook(out)+`"}]}]
		}
	}`), 0o600); err != nil {
		t.Fatalf("write plugin hooks.json: %v", err)
	}
	manager, err := LoadWithSources(
		context.Background(),
		filepath.Join(t.TempDir(), "missing.json"),
		[]ExtraSource{{Path: filepath.Join(pluginDir, "hooks.json"), Dir: pluginDir}},
	)
	if err != nil {
		t.Fatalf("LoadWithSources: %v", err)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{
		"tool":        "exec_command",
		"tool_input":  map[string]any{"command": "secret"},
		"tool_result": map[string]any{"content": "[REDACTED]"},
		"prompt":      "secret prompt",
		"message":     "secret message",
		"target":      "worker",
	})
	data := waitForContent(t, out)
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("hook stdin is not JSON: %v", err)
	}
	if payload["tool"] != "exec_command" {
		t.Fatalf("tool field missing: %+v", payload)
	}
	for _, key := range strippedFields {
		if _, ok := payload[key]; ok {
			t.Fatalf("untrusted payload must not contain %q: %+v", key, payload)
		}
	}
}

func TestTrustedSourceSeesContentFields(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "trusted.out")
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(`{
		"hooks": {
			"PreToolUse": [{"hooks": [{"command": "`+appendHook(out)+`"}]}]
		}
	}`), 0o600); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}
	manager, err := LoadWithSources(
		context.Background(),
		filepath.Join(t.TempDir(), "missing.json"),
		[]ExtraSource{{
			Path:    filepath.Join(dir, "hooks.json"),
			Dir:     dir,
			Trusted: true,
		}},
	)
	if err != nil {
		t.Fatalf("LoadWithSources: %v", err)
	}
	manager.Fire(context.Background(), EventPreToolUse, map[string]any{
		"tool":       "exec_command",
		"tool_input": map[string]any{"command": "ls"},
	})
	data := waitForContent(t, out)
	if !strings.Contains(string(data), `"tool_input"`) {
		t.Fatalf("trusted payload lost its content fields: %s", data)
	}
}

func TestFireIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	requirePosixShell(t)
	path := writeHooks(t, `{
		"hooks": {
			"PreToolUse": [{"hooks": [
				{"command": "echo out; echo err 1>&2", "timeout": 10}
			]}]
		}
	}`)
	manager, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.Fire(context.Background(), EventPreToolUse, map[string]any{"tool": "x"})
		}()
	}
	wg.Wait()
}
