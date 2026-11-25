// Package e2e is the stack test: every module this workspace ships, assembled the way a
// host assembles them, running one conversation.
//
// It exists because each module verifies its own seam against a fake of the module on the
// other side of it. `core` is forbidden from importing an implementation
// (TestCoreDoesNotImportSiblingModules), so its end-to-end test uses a fake backend and
// fake tools; the sandbox and tool modules test themselves against a fake host. Nothing in
// the repository proved that the real local runner, the real file tools, the real tool
// chain, the real graph engine and the real SQLite store agree with each other — and
// agreeing is the only thing a deployment actually needs.
//
// The assembly lives in internal/stack, because test/fault and test/perf assemble the same
// one. What is here is the conversation: a turn that writes a file, and a restart that
// carries the transcript.
//
// It is not a workspace member for the release: it has no changeset and is never tagged.
// What it verifies is the working tree, which is why it is here and not in examples/forge
// (a consumer, pinned to released versions on purpose).
package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/test/internal/stack"
	"github.com/GizClaw/flowcraft/tool/builtin/files"
)

// TestTheStackWritesAFileAndResumes is the whole workspace in one turn: a process document
// and an agent document resolve through the real loader, the real local sandbox backs the
// session, the real file tool writes inside the grant's write set, the real graph engine
// loops model → tools → model, the real SQLite store records the turn, and a second
// process opened on that store continues the conversation.
func TestTheStackWritesAFileAndResumes(t *testing.T) {
	note := filepath.Join(t.TempDir(), "note.txt")

	// The model asks for the file tool once, then answers. The note path is the only thing
	// the script cannot know: it has to be somewhere the grant allows.
	write, err := message.NewToolCall("call-1", files.WriteFileName, map[string]any{
		"file_path": note,
		"content":   "written by the stack test",
	})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}

	s := stack.Open(t,
		stack.Reply{ToolCall: &write},
		stack.Reply{Text: "the note is written"},
	)
	s.OpenSession(t, stack.SessionID)
	s.Run(t, stack.SessionID, "write the note")

	// The file the model asked for exists, with what it asked to be written: the tool ran
	// through the real chain and the session's real tree, not a stub.
	content, err := os.ReadFile(note)
	if err != nil {
		t.Fatalf("read the file the tool wrote: %v", err)
	}
	if string(content) != "written by the stack test" {
		t.Fatalf("file content = %q", content)
	}

	// A second process on the same store: the turn's checkpoint is what the next session
	// carries, so the model sees the first turn's transcript rather than an empty
	// conversation.
	s.Restart(t)
	s.OpenSession(t, stack.SessionID)
	s.Run(t, stack.SessionID, "write another note")

	if got := s.Model.Calls(); got < 3 {
		t.Fatalf("the model was called %d times, want the first turn's two calls and the second's", got)
	}
	requests := s.Model.Requests()
	resumed := requests[len(requests)-1]
	carried := resumed.Input.Content.Text() == "write the note"
	for _, m := range resumed.Context {
		if m.Content.Text() == "write the note" {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the resumed turn's request does not carry the first turn's message: %+v", resumed)
	}
}
