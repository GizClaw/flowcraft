package jsrt_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/agent/scriptrt/jsrt"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func TestFSBridge_RoundTrip(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	ws := mustFSWorkspace(t)
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewFSBridge(ws),
	)
	_, err := rt.Exec(context.Background(), "fs-rt", `
		// File should not exist initially.
		if (fs.exists("notes.txt")) throw new Error("file should not exist yet");

		// Write then read.
		fs.write("notes.txt", "hello world");
		if (!fs.exists("notes.txt")) throw new Error("file should exist after write");
		var contents = fs.read("notes.txt");
		if (contents !== "hello world") throw new Error("read mismatch: " + contents);

		// Overwrite truncates (workspace.Write semantics).
		fs.write("notes.txt", "second");
		if (fs.read("notes.txt") !== "second") throw new Error("overwrite failed");

		// Delete and confirm.
		fs.delete("notes.txt");
		if (fs.exists("notes.txt")) throw new Error("file should be gone after delete");

		board.setVar("ok", true);
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("ok"); v != true {
		t.Fatal("script should have completed all assertions")
	}
}

func TestFSBridge_NilWorkspace_ReadWriteDeleteNotAvailable(t *testing.T) {
	_, raw := bindings.NewFSBridge(nil)(context.Background())
	api := raw.(map[string]any)

	if api["exists"].(func(string) bool)("anything") {
		t.Fatal("nil workspace should report not-exists")
	}
	if _, err := api["read"].(func(string) (string, error))("anything"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("read error = %v, want NotAvailable", err)
	}
	if err := api["write"].(func(string, string) error)("anything", "data"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("write error = %v, want NotAvailable", err)
	}
	if err := api["delete"].(func(string) error)("anything"); !errdefs.IsNotAvailable(err) {
		t.Fatalf("delete error = %v, want NotAvailable", err)
	}
}

func TestFSBridge_ReadError_PropagatesToScript(t *testing.T) {
	// Reading a non-existent file from a real workspace must surface the
	// underlying error to the script rather than swallowing it — confirms
	// the err return path in NewFSBridge.read is wired through.
	rt := jsrt.New(jsrt.WithPoolSize(1))
	ws := mustFSWorkspace(t)
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewFSBridge(ws),
	)
	_, err := rt.Exec(context.Background(), "fs-readmiss", `
		try {
			fs.read("missing.txt");
		} catch (e) {
			board.setVar("caught", true);
			return;
		}
		throw new Error("read of missing file should have thrown");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("caught"); v != true {
		t.Fatal("script should have caught the missing-file error")
	}
}

func TestFSBridge_ReadWriteLimits(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	ws := mustFSWorkspace(t)
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil,
		bindings.NewBoardBridge(board),
		bindings.NewFSBridge(ws,
			bindings.WithMaxReadBytes(4),
			bindings.WithMaxWriteBytes(4)),
	)

	if _, err := rt.Exec(context.Background(), "fs-writecap", `
		fs.write("big.txt", "12345");
	`, env); err == nil {
		t.Fatal("fs.write over the cap should fail")
	}

	// Plant an oversized file directly so fs.read sees it.
	if err := ws.Write(context.Background(), "big.txt", []byte("12345")); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := rt.Exec(context.Background(), "fs-readcap", `
		fs.read("big.txt");
	`, env); err == nil {
		t.Fatal("fs.read over the cap should fail")
	}

	if _, err := rt.Exec(context.Background(), "fs-within-cap", `
		fs.write("ok.txt", "1234");
		if (fs.read("ok.txt") !== "1234") throw new Error("round trip mismatch");
	`, env); err != nil {
		t.Fatalf("within-cap fs ops should succeed: %v", err)
	}
}

func mustFSWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	return ws
}
