package jsrt_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/bindings"
	"github.com/GizClaw/flowcraft/sdkx/agent/jsrt"
)

func TestBoardBridge(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()
	board.SetVar("x", 10)

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "board", `
		var val = board.getVar("x");
		if (val !== 10) throw new Error("expected 10, got " + val);
		board.setVar("y", val * 2);
		if (!board.hasVar("x")) throw new Error("hasVar failed");
		if (board.hasVar("z")) throw new Error("hasVar false positive");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	y, ok := board.GetVar("y")
	if !ok {
		t.Fatal("board should have 'y'")
	}
	if y != int64(20) {
		t.Fatalf("y = %v (type %T), want 20", y, y)
	}
}

func TestBoardBridge_Channel_ReadAfterAppendChannel(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "channel-append", `
		// empty channel reads as empty list (never null)
		var initial = board.channel("main");
		if (initial.length !== 0) throw new Error("initial should be empty, got " + initial.length);

		// appendChannel one user message with a single text part —
		// script-side shape is the inference wire format.
		board.appendChannel("main", {
			role: "user",
			content: { parts: [{ type: "text", text: "hi" }] }
		});

		var msgs = board.channel("main");
		if (msgs.length !== 1) throw new Error("expected 1 message after append");
		if (msgs[0].role !== "user") throw new Error("role lost");
		if (msgs[0].content.parts[0].type !== "text") throw new Error("part type lost");
		if (msgs[0].content.parts[0].text !== "hi") throw new Error("text lost");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBoardBridge_Channel_RoundTripViaSetChannel(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "channel-roundtrip", `
		// Build a multimodal message (text + image), set it on the channel,
		// then re-read and verify nothing was lost.
		board.setChannel("main", [
			{ role: "user", content: { parts: [
				{ type: "text", text: "look at this" },
				{ type: "image", source: { kind: "url", url: "https://x/a.png", media_type: "image/png" } }
			] } }
		]);

		var msgs = board.channel("main");
		if (msgs.length !== 1) throw new Error("expected 1 message");
		var p = msgs[0].content.parts;
		if (p.length !== 2) throw new Error("expected 2 parts, got " + p.length);
		if (p[0].type !== "text" || p[0].text !== "look at this") throw new Error("text part lost");
		if (p[1].type !== "image" || p[1].source.url !== "https://x/a.png") throw new Error("image part lost");
		if (p[1].source.media_type !== "image/png") throw new Error("media_type lost");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBoardBridge_Channel_AppendChannel_ValidationError_ThrowsToScript(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "channel-append-bad", `
		// "parts" at the top level is not a Message field (the wire format
		// nests it under "content") — the strict decoder must throw and
		// name the offending field, not silently append.
		try {
			board.appendChannel("main", { role: "user", parts: [{ type: "text", text: "hi" }] });
		} catch (e) {
			// rethrow only if the error doesn't reference the unknown field —
			// that confirms the path-prefixed error reaches the script.
			if (String(e).indexOf("parts") === -1) throw new Error("error should name 'parts': " + e);
			board.setVar("caught", true);
			return;
		}
		throw new Error("expected appendChannel to throw on unknown field");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("caught"); v != true {
		t.Fatal("script should have caught the validation error")
	}
}

func TestBoardBridge_Channel_SetChannel_RejectsUnknownPartField(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "channel-set-typo", `
		try {
			board.setChannel("main", [
				{ role: "user", content: { parts: [{ type: "text", txt: "typo!" }] } }
			]);
		} catch (e) {
			if (String(e).indexOf("txt") === -1) throw new Error("error should name 'txt': " + e);
			board.setVar("caught", true);
			return;
		}
		throw new Error("expected setChannel to throw on unknown field");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := board.GetVar("caught"); v != true {
		t.Fatal("script should have caught the typo")
	}
	// And the bad batch must NOT have landed on the board.
	if msgs := board.Channel("main"); len(msgs) != 0 {
		t.Errorf("typo'd batch should not be persisted, got %+v", msgs)
	}
}

func TestBoardBridge_Channel_NamedChannelsAreIsolated(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	board := agent.NewBoard()

	env := bindings.BuildEnv(context.Background(), nil, bindings.NewBoardBridge(board))
	_, err := rt.Exec(context.Background(), "channel-named", `
		board.appendChannel("main",   { role: "user", content: { parts: [{ type: "text", text: "a" }] } });
		board.appendChannel("scratch",{ role: "user", content: { parts: [{ type: "text", text: "b" }] } });

		if (board.channel("main").length    !== 1) throw new Error("main count");
		if (board.channel("scratch").length !== 1) throw new Error("scratch count");
		if (board.channel("main")[0].content.parts[0].text    !== "a") throw new Error("main text");
		if (board.channel("scratch")[0].content.parts[0].text !== "b") throw new Error("scratch text");
	`, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
