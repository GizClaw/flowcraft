package exec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
	"github.com/GizClaw/flowcraft/core/tool"
)

func newSessionGroup(t *testing.T, opts ...SessionOption) *SessionGroup {
	t.Helper()
	runner := local.New(t.TempDir())
	t.Cleanup(func() { _ = runner.Close() })
	group, err := NewSession(runner, opts...)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })
	return group
}

func findTool(t *testing.T, group *SessionGroup, name string) tool.Tool {
	t.Helper()
	for _, candidate := range group.Tools() {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func callTool(t *testing.T, candidate tool.Tool, arguments map[string]any) (any, error) {
	t.Helper()
	payload, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content, err := candidate.Execute(context.Background(), string(payload))
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal([]byte(content.Text()), &out); err != nil {
		t.Fatalf("decode %q: %v", content.Text(), err)
	}
	return out, nil
}

// Starting, feeding and waiting is split across two tools that share one handle
// table.
func TestSessionStartFeedWaitClose(t *testing.T) {
	group := newSessionGroup(t)
	lifecycle := findTool(t, group, SessionName)
	stdin := findTool(t, group, WriteStdinName)

	started, err := callTool(t, lifecycle, map[string]any{
		"action": "start",
		"argv":   []string{"/bin/sh", "-c", "echo hi; exit 0"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := started.(map[string]any)["process_id"].(string)

	var output strings.Builder
	var cursor float64
	for {
		read, err := callTool(t, stdin, map[string]any{"process_id": id, "after_seq": cursor})
		if err != nil {
			t.Fatalf("write_stdin: %v", err)
		}
		result := read.(map[string]any)
		for _, raw := range result["chunks"].([]any) {
			output.WriteString(raw.(map[string]any)["data"].(string))
		}
		cursor = result["next_seq"].(float64)
		if result["eof"].(bool) {
			break
		}
	}
	if !strings.Contains(output.String(), "hi") {
		t.Errorf("output = %q, want it to contain hi", output.String())
	}

	if _, err := callTool(t, lifecycle, map[string]any{"action": "wait", "process_id": id}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if _, err := callTool(t, lifecycle, map[string]any{"action": "close", "process_id": id}); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// Feeding stdin reaches the process and closing stdin gives it EOF.
func TestWriteStdinFeedsTheProcess(t *testing.T) {
	group := newSessionGroup(t)
	lifecycle := findTool(t, group, SessionName)
	stdin := findTool(t, group, WriteStdinName)

	started, err := callTool(t, lifecycle, map[string]any{
		"action": "start", "argv": []string{"/bin/cat"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := started.(map[string]any)["process_id"].(string)

	read, err := callTool(t, stdin, map[string]any{
		"process_id": id, "data": "hello\n", "close_stdin": true,
	})
	if err != nil {
		t.Fatalf("write_stdin: %v", err)
	}
	var output strings.Builder
	for _, raw := range read.(map[string]any)["chunks"].([]any) {
		output.WriteString(raw.(map[string]any)["data"].(string))
	}
	if !strings.Contains(output.String(), "hello") {
		t.Errorf("echoed output = %q, want hello", output.String())
	}
}

// A parameter the runner cannot honour is refused, not silently ignored.
func TestTTYRefusedWhenNotPublished(t *testing.T) {
	group := newSessionGroup(t)
	if _, err := callTool(t, findTool(t, group, SessionName), map[string]any{
		"action": "start", "argv": []string{"/bin/sh", "-c", "true"}, "tty": true,
	}); err == nil {
		t.Fatal("tty accepted although the group did not publish TTY support")
	}
}

// kill stops the session through Terminate, which the contract has.
func TestSignalKillStopsTheSession(t *testing.T) {
	group := newSessionGroup(t)
	lifecycle := findTool(t, group, SessionName)

	started, err := callTool(t, lifecycle, map[string]any{
		"action": "start", "argv": []string{"/bin/sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := started.(map[string]any)["process_id"].(string)
	if _, err := callTool(t, lifecycle, map[string]any{
		"action": "signal", "process_id": id, "signal": "kill",
	}); err != nil {
		t.Fatalf("signal kill: %v", err)
	}
	exit, err := callTool(t, lifecycle, map[string]any{
		"action": "wait", "process_id": id, "timeout_seconds": 5,
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if exit.(map[string]any)["reason"] == "exited" {
		t.Errorf("reason = %v, want a kill, not an ordinary exit", exit)
	}
}

func TestSessionStartRequiresArgv(t *testing.T) {
	if _, err := callTool(t, findTool(t, newSessionGroup(t), SessionName), map[string]any{"action": "start"}); err == nil {
		t.Fatal("start without argv accepted")
	}
}

func TestWriteStdinUnknownIDRejected(t *testing.T) {
	if _, err := callTool(t, findTool(t, newSessionGroup(t), WriteStdinName), map[string]any{
		"process_id": "nope",
	}); err == nil {
		t.Fatal("unknown session accepted")
	}
}

// The runner is shared; the group is not. A session started outside this tool
// must not appear in its list, because the model can neither feed nor stop it.
func TestListShowsOnlyOwnedSessions(t *testing.T) {
	runner := local.New(t.TempDir())
	t.Cleanup(func() { _ = runner.Close() })

	foreign, err := runner.Start(context.Background(), sandboxSessionSpec(t))
	if err != nil {
		t.Fatalf("start foreign session: %v", err)
	}
	t.Cleanup(func() { _ = runner.Terminate(context.Background(), foreign.ID()) })

	group, err := NewSession(runner)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })

	payload, err := callTool(t, findTool(t, group, SessionName), map[string]any{
		"action": "start", "argv": []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("start owned session: %v", err)
	}
	own, _ := payload.(map[string]any)
	ownID, _ := own["process_id"].(string)
	if ownID == "" {
		t.Fatalf("start returned no process_id: %v", payload)
	}

	listed, err := callTool(t, findTool(t, group, SessionName), map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := listedIDs(t, listed)
	if !contains(ids, ownID) {
		t.Errorf("list %v omits the session this tool started (%s)", ids, ownID)
	}
	if contains(ids, foreign.ID()) {
		t.Errorf("list %v shows a session another tool started (%s)", ids, foreign.ID())
	}
}

// sandboxSessionSpec is the cheapest session that stays alive long enough to be
// listed.
func sandboxSessionSpec(t *testing.T) sandbox.SessionSpec {
	t.Helper()
	return sandbox.SessionSpec{Argv: []string{"sh", "-c", "sleep 30"}}
}

func listedIDs(t *testing.T, payload any) []string {
	t.Helper()
	entries, ok := payload.([]any)
	if !ok {
		t.Fatalf("list returned %T, want entries", payload)
	}
	ids := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("entry = %T, want an object", raw)
		}
		if id, ok := entry["process_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
