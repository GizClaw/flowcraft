package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/sdkx/claw"
	tea "github.com/charmbracelet/bubbletea"
)

func TestTUIModelRendersThreePaneWorkspace(t *testing.T) {
	model := newTUIModel(nil, "/tmp/workspace")
	model.cfg.Agent.ID = "agent"
	model.cfg.Models.Chat = "chat"
	model.cfg.History.Enabled = true
	model.messages = []tuiChatMessage{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
	}
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	model = next.(tuiModel)
	view := model.View()
	for _, want := range []string{"Recall", "Chat", "Workspace", "Memory", "History", "user: hi", "<node>: hello"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "assistant:") {
		t.Fatalf("view contains unwanted assistant label:\n%s", view)
	}
}

func TestTUIModelTabSwitchesFocus(t *testing.T) {
	model := newTUIModel(nil, "")
	if model.focus != tuiFocusChat {
		t.Fatalf("focus = %d, want chat", model.focus)
	}
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(tuiModel)
	if model.focus != tuiFocusRecall {
		t.Fatalf("focus = %d, want recall", model.focus)
	}
}

func TestTUIModelEnterSubmitsChatInput(t *testing.T) {
	model := newTUIModel(nil, "")
	model.chatInput.SetValue("hello")

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(tuiModel)

	if cmd == nil {
		t.Fatal("cmd is nil, want round start command")
	}
	if !model.running {
		t.Fatal("model.running = false, want true")
	}
	if got := model.chatInput.Value(); got != "" {
		t.Fatalf("chat input = %q, want cleared", got)
	}
	if len(model.messages) != 2 {
		t.Fatalf("messages = %+v, want user + assistant placeholder", model.messages)
	}
	if model.messages[0].Role != "user" || model.messages[0].Text != "hello" {
		t.Fatalf("user message = %+v, want submitted text", model.messages[0])
	}
	if model.messages[1].Role != "assistant" || model.activeIndex != 1 {
		t.Fatalf("assistant placeholder = %+v activeIndex=%d", model.messages[1], model.activeIndex)
	}
}

func TestTUIModelCtrlJSubmitsChatInput(t *testing.T) {
	model := newTUIModel(nil, "")
	model.chatInput.SetValue("hello")

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	model = next.(tuiModel)

	if cmd == nil {
		t.Fatal("cmd is nil, want round start command")
	}
	if len(model.messages) == 0 || model.messages[0].Text != "hello" {
		t.Fatalf("messages = %+v, want submitted chat", model.messages)
	}
}

func TestTUIModelRuneNewlineSubmitsChatInput(t *testing.T) {
	model := newTUIModel(nil, "")
	model.chatInput.SetValue("hello")

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'\r'}})
	model = next.(tuiModel)

	if cmd == nil {
		t.Fatal("cmd is nil, want round start command")
	}
	if len(model.messages) == 0 || model.messages[0].Text != "hello" {
		t.Fatalf("messages = %+v, want submitted chat", model.messages)
	}
}

func TestTUIProgramCarriageReturnSubmitsChatInput(t *testing.T) {
	var out bytes.Buffer
	program := tea.NewProgram(
		newTUIModel(nil, ""),
		tea.WithInput(strings.NewReader("hello\rq")),
		tea.WithOutput(&out),
		tea.WithoutRenderer(),
	)

	finalModel, err := program.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	model := finalModel.(tuiModel)
	if len(model.messages) == 0 || model.messages[0].Role != "user" || model.messages[0].Text != "hello" {
		t.Fatalf("messages = %+v, want terminal-enter submitted chat", model.messages)
	}
}

func TestTUIModelEnterSubmitsRecallInput(t *testing.T) {
	model := newTUIModel(nil, "")
	model.toggleFocus()
	model.recallInput.SetValue("memory")

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(tuiModel)

	if cmd == nil {
		t.Fatal("cmd is nil, want recall command")
	}
	if model.recall.Query != "memory" {
		t.Fatalf("recall query = %q, want memory", model.recall.Query)
	}
	if model.status != "recalling" {
		t.Fatalf("status = %q, want recalling", model.status)
	}
}

func TestTUIModelExtractingStatusBlocksNextChatSubmit(t *testing.T) {
	model := newTUIModel(nil, "")
	model.running = true
	model.chatInput.SetValue("next")
	model.applyRoundEvent(claw.Event{Type: claw.EventStatus, Content: "extracting"})

	next, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(tuiModel)

	if cmd != nil {
		t.Fatal("cmd is non-nil, want no submit while extracting")
	}
	if len(model.messages) != 0 {
		t.Fatalf("messages = %+v, want no new user message while extracting", model.messages)
	}
	if model.status != "extracting; wait or q to stop" {
		t.Fatalf("status = %q, want extracting wait prompt", model.status)
	}
}

func TestTUIModelQQuitsEvenWithInput(t *testing.T) {
	model := newTUIModel(nil, "")
	model.chatInput.SetValue("draft")
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assertTeaQuit(t, cmd)
}

func TestTUIModelEscQuitsWhileRunning(t *testing.T) {
	model := newTUIModel(nil, "")
	model.running = true
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	assertTeaQuit(t, cmd)
}

func TestTUIModelRecallZeroHitsShowsQueryFeedback(t *testing.T) {
	model := newTUIModel(nil, "")
	next, _ := model.Update(tuiRecallMsg{result: tuiRecallResult{
		Enabled: true,
		Query:   "memory",
		Hits:    nil,
	}})
	model = next.(tuiModel)
	view := model.View()
	if !strings.Contains(view, `No recall hits for "memory".`) {
		t.Fatalf("view missing recall feedback:\n%s", view)
	}
	if !strings.Contains(view, "status=recall: 0 hits") {
		t.Fatalf("view missing recall status:\n%s", view)
	}
}

func TestShouldTUIAutoStartOnlyForEmptySelfStartingConversation(t *testing.T) {
	cfg := claw.Config{}
	cfg.Conversation.Starts = "self"
	if !shouldTUIAutoStart(cfg, 0) {
		t.Fatal("self-starting empty conversation should auto start")
	}
	if shouldTUIAutoStart(cfg, 1) {
		t.Fatal("conversation with history should not auto start")
	}
	cfg.Conversation.Starts = "peer"
	if shouldTUIAutoStart(cfg, 0) {
		t.Fatal("peer-starting conversation should not auto start")
	}
}

func TestTUIModelRendersAssistantNodeID(t *testing.T) {
	model := newTUIModel(nil, "")
	model.messages = []tuiChatMessage{{Role: "assistant", Text: "hello", NodeID: "answer_node"}}
	view := model.View()
	if !strings.Contains(view, "<answer_node>: hello") {
		t.Fatalf("view missing node id:\n%s", view)
	}
	if strings.Contains(view, "assistant:") || strings.Contains(view, "answer_node:") || strings.Contains(view, "[answer_node]") {
		t.Fatalf("view contains unwanted assistant/node label:\n%s", view)
	}
}

func TestTUIModelSplitsAssistantOutputByNodeID(t *testing.T) {
	model := newTUIModel(nil, "")
	model.messages = []tuiChatMessage{{Role: "assistant"}}
	model.activeIndex = 0
	model.applyRoundEvent(clawEventToken("node_a", "hello"))
	model.applyRoundEvent(clawEventToken("node_b", "world"))
	if len(model.messages) != 2 {
		t.Fatalf("messages = %+v, want two node messages", model.messages)
	}
	if model.messages[0].NodeID != "node_a" || model.messages[0].Text != "hello" {
		t.Fatalf("first message = %+v", model.messages[0])
	}
	if model.messages[1].NodeID != "node_b" || model.messages[1].Text != "world" {
		t.Fatalf("second message = %+v", model.messages[1])
	}
}

func TestTUIHistorySkipsLegacyInternalCaseAction(t *testing.T) {
	if !isInternalHistoryMessage("assistant", "", "case_action: kind=other; target=none") {
		t.Fatal("legacy internal case_action should be filtered")
	}
	if isInternalHistoryMessage("assistant", "generate_game_master", "case_action: kind=start; target=game_start") {
		t.Fatal("named assistant messages should not be treated as legacy internal history")
	}
	if isInternalHistoryMessage("user", "", "case_action: kind=start; target=game_start") {
		t.Fatal("user messages should not be filtered")
	}
}

func TestWrapLineDoesNotSplitUTF8(t *testing.T) {
	lines := wrapLine("你好世界", 2)
	want := []string{"你好", "世界"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %#v, want %#v", lines, want)
	}
	for _, line := range lines {
		if !utf8.ValidString(line) {
			t.Fatalf("line is not valid utf8: %q", line)
		}
	}
}

func assertTeaQuit(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("cmd is nil, want tea.Quit")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("cmd message = %T, want tea.QuitMsg", msg)
	}
}

func clawEventToken(nodeID, content string) claw.Event {
	return claw.Event{Type: claw.EventToken, NodeID: nodeID, Content: content}
}
