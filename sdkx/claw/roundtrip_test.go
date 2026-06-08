package claw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestRoundTripStreamsTokensAndResult(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "hello there"}, nil)
	defer app.Close()

	resp, err := app.RoundTrip(Request{Text: "hi"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	var tokens strings.Builder
	var sawResult bool
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Type {
		case EventToken:
			tokens.WriteString(ev.Content)
		case EventResult:
			sawResult = ev.Result != nil
		case EventError:
			t.Fatalf("unexpected error event: %s", ev.Err)
		}
	}
	if got := tokens.String(); got != "hello there" {
		t.Fatalf("tokens = %q, want %q", got, "hello there")
	}
	if !sawResult {
		t.Fatal("missing result event")
	}
}

func TestRoundTripInterruptsConcurrentSameContext(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	llm := &blockingLLM{release: make(chan struct{})}
	app := newTestClaw(t, ws, llm, nil)
	defer app.Close()

	resp, err := app.RoundTrip(Request{Text: "hi"})
	if err != nil {
		t.Fatalf("RoundTrip first: %v", err)
	}
	resp2, err := app.RoundTrip(Request{Text: "again"})
	if err != nil {
		t.Fatalf("RoundTrip second: %v", err)
	}
	close(llm.release)
	deadline := time.After(2 * time.Second)
	var firstInterrupted bool
	for {
		select {
		case <-deadline:
			t.Fatal("first response did not finish")
		default:
		}
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		} else if ev.Type == EventError {
			firstInterrupted = true
		}
	}
	if !firstInterrupted {
		t.Fatal("first response was not interrupted")
	}
	for {
		if _, err := resp2.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next second: %v", err)
		}
	}
}

func TestRoundTripPersistsContextState(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, nil)
	defer app.Close()

	resp, err := app.RoundTrip(Request{
		Text:   "continue",
		Inputs: map[string]any{"current_arc": "arc_02_heaven"},
	})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	for {
		if _, err := resp.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	st, err := app.loadContextState(context.Background(), app.contextID())
	if err != nil {
		t.Fatalf("loadContextState: %v", err)
	}
	if st.Vars["current_arc"] != "arc_02_heaven" {
		t.Fatalf("current_arc = %v, want arc_02_heaven", st.Vars["current_arc"])
	}
}

func TestRoundTripLoadsConfiguredHistoryByContextID(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	llm := &capturingLLM{replies: []string{"first reply", "second reply"}}
	app := newTestClaw(t, ws, llm, func(cfg *Config) {
		cfg.History.Enabled = true
		cfg.History.Kind = "buffer"
		cfg.History.MaxMessages = 10
	})
	defer app.Close()

	resp, err := app.RoundTrip(Request{Text: "first question"})
	if err != nil {
		t.Fatalf("RoundTrip first: %v", err)
	}
	for {
		if _, err := resp.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next first: %v", err)
		}
	}

	resp, err = app.RoundTrip(Request{Text: "second question"})
	if err != nil {
		t.Fatalf("RoundTrip second: %v", err)
	}
	for {
		if _, err := resp.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next second: %v", err)
		}
	}

	calls := llm.calls()
	if len(calls) != 2 {
		t.Fatalf("captured calls = %d, want 2", len(calls))
	}
	second := calls[1]
	if len(second) != 3 {
		t.Fatalf("second call messages = %d, want 3: %#v", len(second), second)
	}
	if second[0].Role != model.RoleUser || second[0].Content() != "first question" {
		t.Fatalf("message 0 = (%s, %q), want first user turn", second[0].Role, second[0].Content())
	}
	if second[1].Role != model.RoleAssistant || second[1].Content() != "first reply" {
		t.Fatalf("message 1 = (%s, %q), want first assistant turn", second[1].Role, second[1].Content())
	}
	if second[2].Role != model.RoleUser || second[2].Content() != "second question" {
		t.Fatalf("message 2 = (%s, %q), want second user turn", second[2].Role, second[2].Content())
	}
}

func TestRoundTripPersistsStateNodeVars(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, func(cfg *Config) {
		cfg.Agent.Graph = graph.GraphDefinition{
			Name:  "stateful-story",
			Entry: "route",
			Nodes: []graph.NodeDefinition{
				{
					ID:   "route",
					Type: "script",
					Config: map[string]any{
						"source": `board.setVar("current_chapter", 0);`,
					},
				},
				{
					ID:   "storyteller_origin",
					Type: "llm",
					Config: map[string]any{
						"model": "chat",
					},
				},
				{
					ID:   "state_after_origin",
					Type: "script",
					Config: map[string]any{
						"source": `board.setVar("current_chapter", 7); board.setVar("tmp_current_arc", "storyteller_origin");`,
					},
				},
			},
			Edges: []graph.EdgeDefinition{
				{From: "route", To: "storyteller_origin"},
				{From: "storyteller_origin", To: "state_after_origin"},
				{From: "state_after_origin", To: graph.END},
			},
		}
	})
	defer app.Close()

	resp, err := app.RoundTrip(Request{Text: "continue"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	for {
		if _, err := resp.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	st, err := app.loadContextState(context.Background(), app.contextID())
	if err != nil {
		t.Fatalf("loadContextState: %v", err)
	}
	if st.Vars["current_chapter"] != float64(7) {
		t.Fatalf("current_chapter = %v, want 7", st.Vars["current_chapter"])
	}
	if _, ok := st.Vars["tmp_current_arc"]; ok {
		t.Fatal("tmp_current_arc should not be persisted")
	}
}

func TestRoundTripEmitsToolEventsWithNodeID(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, toolCallLLM{}, func(cfg *Config) {
		publish := true
		cfg.Agent.Tools = ToolConfigs{{Name: setDeviceVolumeToolName}}
		cfg.Agent.Publisher.Nodes = map[string]NodePublishConfig{
			"function_adjust_device_volume": {Publish: &publish},
		}
		cfg.Agent.Graph = graph.GraphDefinition{
			Name:  "tool-test",
			Entry: "function_adjust_device_volume",
			Nodes: []graph.NodeDefinition{
				{
					ID:   "function_adjust_device_volume",
					Type: "llm",
					Config: map[string]any{
						"model":      "chat",
						"tool_names": []string{setDeviceVolumeToolName},
					},
				},
			},
			Edges: []graph.EdgeDefinition{
				{From: "function_adjust_device_volume", To: graph.END},
			},
		}
	})
	defer app.Close()
	app.Handle(setDeviceVolumeToolName, func(_ context.Context, name string, args json.RawMessage) (string, error) {
		return fmt.Sprintf(`{"ok":true,"action":%q,"args":%s}`, name, string(args)), nil
	})

	resp, err := app.RoundTrip(Request{Text: "调整音量到42"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	var sawCall, sawResult bool
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Type {
		case EventToolCall:
			sawCall = true
			if ev.NodeID != "function_adjust_device_volume" {
				t.Fatalf("tool call node_id = %q", ev.NodeID)
			}
			if ev.Name != setDeviceVolumeToolName {
				t.Fatalf("tool call name = %q", ev.Name)
			}
		case EventToolResult:
			sawResult = true
			if ev.NodeID != "function_adjust_device_volume" {
				t.Fatalf("tool result node_id = %q", ev.NodeID)
			}
			if ev.Name != setDeviceVolumeToolName || !strings.Contains(ev.Content, `"ok":true`) {
				t.Fatalf("tool result = %+v", ev)
			}
		case EventError:
			t.Fatalf("unexpected error event: %s", ev.Err)
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("sawCall=%t sawResult=%t, want both", sawCall, sawResult)
	}
}

func TestResponseInterruptControlsPartialMemory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		discard bool
	}{
		{name: "discard", discard: true},
		{name: "commit_partial", discard: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := workspace.NewLocalWorkspace(t.TempDir())
			if err != nil {
				t.Fatalf("NewLocalWorkspace: %v", err)
			}
			llm := &controlledStreamLLM{
				chunks:     []string{"hello ", "world ", "again"},
				blockAfter: 2,
				blocked:    make(chan struct{}),
			}
			app := newTestClaw(t, ws, llm, nil)
			defer app.Close()
			mem := &recordingMemory{}
			app.memory = &memoryRuntime{
				mem:   mem,
				scope: recall.Scope{RuntimeID: "rt", UserID: "u", AgentID: "a"},
				cfg: MemoryConfig{
					Write: MemoryWriteConfig{SaveConversation: true},
				},
			}

			resp, err := app.RoundTrip(Request{Text: "tell me"})
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			select {
			case <-llm.blocked:
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not reach blocking point")
			}

			ev, err := resp.Next()
			if err != nil {
				t.Fatalf("first Next: %v", err)
			}
			if ev.Type != EventToken || ev.Content != "hello " {
				t.Fatalf("first event = %+v, want hello token", ev)
			}
			readBeforeInterrupt := ev.Content
			if err := resp.Interrupt(tc.discard); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}

			var resultText string
			for {
				ev, err := resp.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next after interrupt: %v", err)
				}
				if ev.Result != nil {
					resultText = latestAssistant(ev.Result.Messages).Content()
				}
			}

			if !strings.Contains(resultText, "world") {
				t.Fatalf("interrupted result text = %q, want SDK result to include buffered token beyond read %q", resultText, readBeforeInterrupt)
			}
			if strings.Contains(readBeforeInterrupt, "world") {
				t.Fatalf("read-before-interrupt text unexpectedly contains world: %q", readBeforeInterrupt)
			}

			saves := mem.savedTurns()
			if tc.discard {
				if len(saves) != 0 {
					t.Fatalf("discard=true persisted %d memory saves", len(saves))
				}
				return
			}
			if len(saves) != 1 {
				t.Fatalf("discard=false memory saves = %d, want 1", len(saves))
			}
			if len(saves[0]) != 2 {
				t.Fatalf("saved turns = %d, want user + assistant", len(saves[0]))
			}
			if got := saves[0][1].Text; got != "hello " {
				t.Fatalf("saved assistant partial = %q, want only read text %q", got, readBeforeInterrupt)
			}
		})
	}
}

func TestBoardSeederCanInjectRecallMemoryIntoBoardVar(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, nil)
	defer app.Close()

	mem := &recordingMemory{
		hits: []recall.Hit{
			{Fact: recall.TemporalFact{Content: "persona_preferences: Tom likes fast scenes."}},
			{Fact: recall.TemporalFact{Content: "story_progress: Monkey returned home."}},
		},
	}
	app.memory = &memoryRuntime{
		mem:   mem,
		scope: recall.Scope{RuntimeID: "rt", UserID: "u"},
		cfg: MemoryConfig{Recall: MemoryRecallConfig{
			Enabled:  true,
			TopK:     5,
			Inject:   "board",
			BoardVar: "persona_memory",
			Query: MemoryRecallQueryConfig{
				Text:  "${input}",
				Lanes: []string{"persona_preferences"},
				Kinds: []string{"preference"},
			},
			Render: MemoryRecallRenderConfig{
				Header:     "Persona memory:",
				ItemPrefix: "* ",
				MaxItems:   1,
			},
		}},
	}

	board, err := app.boardSeeder().SeedBoard(context.Background(), agent.RunInfo{}, &agent.Request{
		ContextID: "ctx",
		Message:   model.NewTextMessage(model.RoleUser, "what happens next"),
	})
	if err != nil {
		t.Fatalf("SeedBoard: %v", err)
	}
	if got := board.GetVarString("persona_memory"); !strings.Contains(got, "Tom likes fast scenes") {
		t.Fatalf("persona_memory = %q, want recalled memory", got)
	} else if strings.Contains(got, "Monkey returned home") {
		t.Fatalf("persona_memory included filtered lane: %q", got)
	} else if !strings.HasPrefix(got, "Persona memory:\n* ") {
		t.Fatalf("persona_memory render = %q, want configured header and prefix", got)
	}
	for _, msg := range board.Channel(engine.MainChannel) {
		if strings.Contains(msg.Content(), "Relevant memory") {
			t.Fatalf("memory was injected into main channel: %+v", msg)
		}
	}
	if got := mem.lastQuery(); got.Text != "what happens next" || len(got.Kinds) != 1 || got.Kinds[0] != recall.FactPreference {
		t.Fatalf("recall query = %+v, want input text and preference kind", got)
	}
}

func TestBoardSeederWritesMultipleRecallProfiles(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, nil)
	defer app.Close()

	mem := &recordingMemory{
		hits: []recall.Hit{
			{Fact: recall.TemporalFact{Content: "story_progress: Monkey left the cave."}},
			{Fact: recall.TemporalFact{Content: "user_preferences: Tom likes faster scenes."}},
		},
	}
	app.memory = &memoryRuntime{
		mem:   mem,
		scope: recall.Scope{RuntimeID: "rt", UserID: "u"},
		cfg: MemoryConfig{Recall: MemoryRecallConfig{
			Enabled: true,
			Profiles: map[string]MemoryRecallProfileConfig{
				"story": {
					Output: "story_memory",
					Query: MemoryRecallQueryConfig{
						Text:  "input",
						Lanes: []string{"story_progress"},
					},
					Render: MemoryRecallRenderConfig{Header: "Story memory:"},
				},
				"user": {
					Output: "user_memory",
					Query: MemoryRecallQueryConfig{
						Text:  "input",
						Lanes: []string{"user_preferences"},
					},
					Render: MemoryRecallRenderConfig{Header: "User memory:"},
				},
				"empty": {
					Output: "empty_memory",
					Query: MemoryRecallQueryConfig{
						Text:  "input",
						Lanes: []string{"missing_lane"},
					},
				},
			},
		}},
	}

	board, err := app.boardSeeder().SeedBoard(context.Background(), agent.RunInfo{}, &agent.Request{
		ContextID: "ctx",
		Message:   model.NewTextMessage(model.RoleUser, "continue"),
	})
	if err != nil {
		t.Fatalf("SeedBoard: %v", err)
	}
	if got := board.GetVarString("story_memory"); !strings.Contains(got, "Monkey left the cave") || strings.Contains(got, "Tom likes") {
		t.Fatalf("story_memory = %q", got)
	}
	if got := board.GetVarString("user_memory"); !strings.Contains(got, "Tom likes faster scenes") || strings.Contains(got, "Monkey left") {
		t.Fatalf("user_memory = %q", got)
	}
	if got := board.GetVarString("empty_memory"); got != "" {
		t.Fatalf("empty_memory = %q, want empty placeholder", got)
	}
	for _, msg := range board.Channel(engine.MainChannel) {
		if strings.Contains(msg.Content(), "Story memory") || strings.Contains(msg.Content(), "User memory") {
			t.Fatalf("profile memory was injected into main channel: %+v", msg)
		}
	}
}

func TestBoardSeederSkipsEmptyRecallProfileQuery(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, nil)
	defer app.Close()

	mem := &recordingMemory{}
	app.memory = &memoryRuntime{
		mem:   mem,
		scope: recall.Scope{RuntimeID: "rt", UserID: "u"},
		cfg: MemoryConfig{Recall: MemoryRecallConfig{
			Enabled: true,
			Profiles: map[string]MemoryRecallProfileConfig{
				"story": {
					Output: "story_memory",
					Query: MemoryRecallQueryConfig{
						Text:  "input",
						Kinds: []string{"state"},
					},
				},
			},
		}},
	}

	board, err := app.boardSeeder().SeedBoard(context.Background(), agent.RunInfo{}, &agent.Request{
		ContextID: "ctx",
		Message:   model.NewTextMessage(model.RoleUser, ""),
	})
	if err != nil {
		t.Fatalf("SeedBoard: %v", err)
	}
	if got := board.GetVarString("story_memory"); got != "" {
		t.Fatalf("story_memory = %q, want empty placeholder", got)
	}
	if mem.recallCalls() != 0 {
		t.Fatalf("Recall calls = %d, want 0 for empty profile query", mem.recallCalls())
	}
}

func TestRoundStreamMux_BuffersSpeculativeBranchesUntilTerminal(t *testing.T) {
	events := make(chan Event, 8)
	publishTrue := true
	mux := newRoundStreamMux(context.Background(), events, PublisherConfig{
		Nodes: map[string]NodePublishConfig{
			"answer_b": {Publish: &publishTrue},
		},
	})

	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.answer_a", map[string]any{
		"type":        "token",
		"content":     "drop me",
		"speculative": true,
		"fork_id":     "run:start",
		"branch_id":   "answer_a",
	})); err != nil {
		t.Fatalf("publish canceled token: %v", err)
	}
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("speculative token leaked before terminal: %+v", got)
	}
	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.answer_a", map[string]any{
		"type":        "parallel_branch_cancel",
		"speculative": true,
		"fork_id":     "run:start",
		"branch_id":   "answer_a",
		"reason":      "route selected another branch",
	})); err != nil {
		t.Fatalf("publish cancel: %v", err)
	}
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("canceled branch emitted events: %+v", got)
	}

	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.answer_b", map[string]any{
		"type":        "token",
		"content":     "keep me",
		"speculative": true,
		"fork_id":     "run:start",
		"branch_id":   "answer_b",
	})); err != nil {
		t.Fatalf("publish accepted token: %v", err)
	}
	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.answer_b", map[string]any{
		"type":        "parallel_branch_accept",
		"speculative": true,
		"fork_id":     "run:start",
		"branch_id":   "answer_b",
	})); err != nil {
		t.Fatalf("publish accept: %v", err)
	}
	got := drainEvents(events)
	if len(got) != 1 {
		t.Fatalf("accepted branch events = %+v, want one token", got)
	}
	if got[0].Type != EventToken || got[0].Content != "keep me" || got[0].NodeID != "answer_b" {
		t.Fatalf("flushed event = %+v", got[0])
	}
}

func TestRoundStreamMux_FiltersUnpublishedNodeTokens(t *testing.T) {
	events := make(chan Event, 8)
	publishFalse := false
	publishTrue := true
	mux := newRoundStreamMux(context.Background(), events, PublisherConfig{
		Nodes: map[string]NodePublishConfig{
			"format_intent":       {Publish: &publishFalse},
			"function_read_story": {Publish: &publishTrue},
		},
	})

	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.format_intent", map[string]any{
		"type":    "token",
		"content": "read_story: series=西游记",
	})); err != nil {
		t.Fatalf("publish hidden token: %v", err)
	}
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("unpublished node emitted events: %+v", got)
	}

	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.function_read_story", map[string]any{
		"type":    "token",
		"content": "好的",
	})); err != nil {
		t.Fatalf("publish visible token: %v", err)
	}
	got := drainEvents(events)
	if len(got) != 1 || got[0].Content != "好的" || got[0].NodeID != "function_read_story" {
		t.Fatalf("visible node events = %+v, want function_read_story token", got)
	}
}

func TestRoundStreamMux_DefaultsNodePublishToFalse(t *testing.T) {
	events := make(chan Event, 8)
	mux := newRoundStreamMux(context.Background(), events, PublisherConfig{})

	if err := mux.Publish(context.Background(), streamEnvelope(t, "run", "agent.node.unconfigured", map[string]any{
		"type":    "token",
		"content": "hidden by default",
	})); err != nil {
		t.Fatalf("publish token: %v", err)
	}
	if got := drainEvents(events); len(got) != 0 {
		t.Fatalf("unconfigured node emitted events: %+v", got)
	}
}

func streamEnvelope(t *testing.T, runID, stepActor string, payload map[string]any) event.Envelope {
	t.Helper()
	env, err := event.NewEnvelope(context.Background(), engine.SubjectStreamDelta(runID, stepActor), payload)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	env.SetRunID(runID)
	agentID, nodeID := splitTestStepActor(stepActor)
	if agentID != "" {
		env.SetAgentID(agentID)
	}
	if nodeID != "" {
		env.SetNodeID(nodeID)
	}
	return env
}

func splitTestStepActor(stepActor string) (agentID, nodeID string) {
	const nodeSep = ".node."
	idx := strings.Index(stepActor, nodeSep)
	if idx < 0 {
		return stepActor, ""
	}
	return stepActor[:idx], stepActor[idx+len(nodeSep):]
}

func drainEvents(events <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

type staticLLM struct {
	reply string
}

func (s staticLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	msg := llm.NewTextMessage(llm.RoleAssistant, s.reply)
	return msg, llm.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, nil
}

func (s staticLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg := llm.NewTextMessage(llm.RoleAssistant, s.reply)
	return &sliceStream{msg: msg, chunks: []string{s.reply}}, nil
}

type capturingLLM struct {
	mu      sync.Mutex
	replies []string
	seen    [][]llm.Message
}

func (c *capturingLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return llm.Message{}, llm.TokenUsage{}, nil
}

func (c *capturingLLM) GenerateStream(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	c.mu.Lock()
	reply := "ok"
	if len(c.seen) < len(c.replies) {
		reply = c.replies[len(c.seen)]
	}
	c.seen = append(c.seen, model.CloneMessages(msgs))
	c.mu.Unlock()
	msg := llm.NewTextMessage(llm.RoleAssistant, reply)
	return &sliceStream{msg: msg, chunks: []string{reply}}, nil
}

func (c *capturingLLM) calls() [][]llm.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]llm.Message, len(c.seen))
	for i, msgs := range c.seen {
		out[i] = model.CloneMessages(msgs)
	}
	return out
}

type toolCallLLM struct{}

func (toolCallLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return model.Message{}, model.TokenUsage{}, nil
}

func (toolCallLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	call := model.ToolCall{ID: "call_volume", Name: setDeviceVolumeToolName, Arguments: `{"pct":42}`}
	msg := model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{Type: model.PartText, Text: "正在调整音量。"},
			{Type: model.PartToolCall, ToolCall: &call},
		},
	}
	return &sliceStream{msg: msg, chunks: []string{"正在调整音量。"}}, nil
}

type blockingLLM struct {
	release chan struct{}
}

func (b *blockingLLM) Generate(ctx context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return llm.Message{}, llm.TokenUsage{}, ctx.Err()
	}
	msg := llm.NewTextMessage(llm.RoleAssistant, "done")
	return msg, llm.TokenUsage{}, nil
}

func (b *blockingLLM) GenerateStream(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	msg, usage, err := b.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	return llm.NewOneChunkStream(msg, usage), nil
}

type sliceStream struct {
	msg    llm.Message
	chunks []string
	idx    int
	cur    model.StreamChunk
}

func (s *sliceStream) Next() bool {
	if s.idx >= len(s.chunks) {
		return false
	}
	s.cur = model.StreamChunk{Role: model.RoleAssistant, Content: s.chunks[s.idx]}
	s.idx++
	return true
}

func (s *sliceStream) Current() model.StreamChunk { return s.cur }
func (s *sliceStream) Err() error                 { return nil }
func (s *sliceStream) Close() error               { return nil }
func (s *sliceStream) Message() model.Message     { return s.msg }
func (s *sliceStream) Usage() model.Usage         { return model.Usage{} }

type controlledStreamLLM struct {
	chunks     []string
	blockAfter int
	blocked    chan struct{}
}

func (l *controlledStreamLLM) Generate(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return model.Message{}, model.TokenUsage{}, nil
}

func (l *controlledStreamLLM) GenerateStream(ctx context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return &controlledStream{
		ctx:        ctx,
		chunks:     l.chunks,
		blockAfter: l.blockAfter,
		blocked:    l.blocked,
	}, nil
}

type controlledStream struct {
	ctx        context.Context
	chunks     []string
	blockAfter int
	blocked    chan struct{}
	idx        int
	cur        model.StreamChunk
	blockOnce  sync.Once
}

func (s *controlledStream) Next() bool {
	if s.idx >= len(s.chunks) {
		return false
	}
	if s.blockAfter > 0 && s.idx >= s.blockAfter {
		s.blockOnce.Do(func() { close(s.blocked) })
		<-s.ctx.Done()
		return false
	}
	s.cur = model.StreamChunk{Role: model.RoleAssistant, Content: s.chunks[s.idx]}
	s.idx++
	return true
}

func (s *controlledStream) Current() model.StreamChunk { return s.cur }
func (s *controlledStream) Err() error                 { return nil }
func (s *controlledStream) Close() error               { return nil }
func (s *controlledStream) Message() model.Message     { return model.Message{} }
func (s *controlledStream) Usage() model.Usage         { return model.Usage{} }

type recordingMemory struct {
	mu          sync.Mutex
	saves       []recall.SaveRequest
	hits        []recall.Hit
	query       recall.Query
	recallCount int
}

func (m *recordingMemory) Save(_ context.Context, _ recall.Scope, req recall.SaveRequest) (recall.SaveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves = append(m.saves, req)
	return recall.SaveResult{}, nil
}

func (m *recordingMemory) savedTurns() [][]recall.TurnContext {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]recall.TurnContext, len(m.saves))
	for i, save := range m.saves {
		out[i] = append([]recall.TurnContext(nil), save.Turns...)
	}
	return out
}

func (m *recordingMemory) Recall(_ context.Context, _ recall.Scope, query recall.Query) ([]recall.Hit, error) {
	m.mu.Lock()
	m.query = query
	m.recallCount++
	m.mu.Unlock()
	return append([]recall.Hit(nil), m.hits...), nil
}

func (m *recordingMemory) lastQuery() recall.Query {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.query
}

func (m *recordingMemory) recallCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recallCount
}

func (m *recordingMemory) Forget(context.Context, recall.Scope, string, ...recall.ForgetMode) error {
	return nil
}

func (m *recordingMemory) ForgetAll(context.Context, recall.Scope, recall.ForgetMode, string) (int, error) {
	return 0, nil
}

func (m *recordingMemory) ExpireRetired(context.Context, recall.Scope, time.Time) (int, error) {
	return 0, nil
}

func (m *recordingMemory) History(context.Context, recall.Scope, string) ([]recall.FactVersion, error) {
	return nil, nil
}

func (m *recordingMemory) Lineage(context.Context, recall.Scope, string) ([]recall.FactLineageNode, error) {
	return nil, nil
}

func (m *recordingMemory) Fork(context.Context, recall.Scope, string, recall.TemporalFact) (recall.SaveResult, error) {
	return recall.SaveResult{}, nil
}

func (m *recordingMemory) Contest(context.Context, recall.Scope, string, []recall.EvidenceRef) (recall.SaveResult, error) {
	return recall.SaveResult{}, nil
}

func (m *recordingMemory) Reinforce(context.Context, recall.Scope, string, float64) error {
	return nil
}

func (m *recordingMemory) Penalize(context.Context, recall.Scope, string, float64) error {
	return nil
}

func (m *recordingMemory) Close() error { return nil }
