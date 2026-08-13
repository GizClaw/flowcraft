package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

func TestChatContentKeepsAllMessages(t *testing.T) {
	m := Model{messages: []chatMessage{
		{Role: "user", Text: "你好"},
		{Role: "tool", Kind: "tool_call", Text: "play_music {\"volume\":1}"},
		{Role: "assistant", Text: "好的"},
	}}
	got := m.chatContent(80)
	for _, want := range []string{
		"user: 你好",
		"[工具调用] play_music {\"volume\":1}",
		"assistant: 好的",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("chatContent missing %q:\n%s", want, got)
		}
	}
}

func TestChatViewportKeepsAllMessagesAndFollowsBottom(t *testing.T) {
	messages := make([]chatMessage, 100)
	for i := range messages {
		messages[i] = chatMessage{Role: "user", Text: fmt.Sprintf("message %d", i)}
	}
	m := Model{messages: messages, chatViewport: viewport.New(0, 0)}
	vp := m.chatViewportFor(60, 12)
	if want := len(messages) * 2; vp.TotalLineCount() != want {
		t.Fatalf("TotalLineCount = %d, want %d", vp.TotalLineCount(), want)
	}
	if !vp.AtBottom() {
		t.Fatalf("viewport should follow the latest content, YOffset = %d", vp.YOffset)
	}
}

func TestChatViewportScrollsWithArrowKeys(t *testing.T) {
	messages := make([]chatMessage, 100)
	for i := range messages {
		messages[i] = chatMessage{Role: "user", Text: fmt.Sprintf("message %d", i)}
	}
	m := NewModel(nil, "workspace")
	m.messages = messages
	m.syncChatViewport(60, 12)
	bottom := m.chatViewport.YOffset

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.chatViewport.YOffset >= bottom {
		t.Fatalf("arrow up did not scroll up: YOffset = %d, want < %d", m.chatViewport.YOffset, bottom)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if m.chatViewport.YOffset != bottom {
		t.Fatalf("arrow down did not scroll back: YOffset = %d, want %d", m.chatViewport.YOffset, bottom)
	}
}

func TestChatViewFitsPanelHeight(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.err = "boom"
	got := m.chatView(60, 12)
	if lines := strings.Split(got, "\n"); len(lines) != 12 {
		t.Fatalf("chatView rendered %d lines, want 12:\n%s", len(lines), got)
	}
}

func TestChatViewShowsLoadingWhileRunning(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.running = true
	got := m.chatView(60, 12)
	if !strings.Contains(got, "agent running") {
		t.Fatalf("chatView should show a loading hint while running:\n%s", got)
	}
}

func TestChatSpinnerAdvancesWhileRunning(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.running = true
	before := m.chatSpinner.View()

	updated, _ := m.Update(spinner.TickMsg{ID: m.chatSpinner.ID()})
	after := updated.(Model).chatSpinner.View()
	if after == before {
		t.Fatalf("spinner did not advance: %q", after)
	}
}

func TestAfterRunRefocusesChatInput(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.chatInput.Blur()
	m.afterRun()
	if !m.chatInput.Focused() {
		t.Fatal("chat input should be focused again after a run finishes")
	}
}

func TestUsageShownBelowInput(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.usage.Calls = 1
	m.usage.InputTokens = 10
	m.usage.OutputTokens = 20
	m.usage.TotalTokens = 30
	got := m.chatView(80, 16)
	if !strings.Contains(got, "usage in 10 out 20 total 30") {
		t.Fatalf("chatView should show token usage below the input:\n%s", got)
	}
}

func TestChatViewFitsPanelHeightWithWrappedUsage(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.usage.Calls = 1
	m.usage.InputTokens = 10
	m.usage.OutputTokens = 20
	m.usage.TotalTokens = 30
	got := m.chatView(32, 14)
	if lines := strings.Split(got, "\n"); len(lines) != 14 {
		t.Fatalf("chatView rendered %d lines, want 14:\n%s", len(lines), got)
	}
}

func TestCtrlCRequiresSecondPress(t *testing.T) {
	m := NewModel(nil, "workspace")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	armed := updated.(Model)
	if !armed.quitArmed {
		t.Fatal("first ctrl+c should arm quit confirmation")
	}
	if cmd == nil {
		t.Fatal("first ctrl+c should schedule the disarm timer")
	}

	_, cmd = armed.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("second ctrl+c should quit")
	}
}

func TestQuitConfirmationDisarms(t *testing.T) {
	m := NewModel(nil, "workspace")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	armed := updated.(Model)

	updated, _ = armed.Update(quitDisarmMsg{})
	if updated.(Model).quitArmed {
		t.Fatal("quit confirmation should disarm on timeout")
	}

	updated, _ = armed.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if updated.(Model).quitArmed {
		t.Fatal("any other key should disarm quit confirmation")
	}
}

func TestEscClearsFocusedInput(t *testing.T) {
	m := NewModel(nil, "workspace")
	m.chatInput.SetValue("hello")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := updated.(Model).chatInput.Value(); got != "" {
		t.Fatalf("esc should clear the chat input, got %q", got)
	}
}

func TestQIsTypable(t *testing.T) {
	m := NewModel(nil, "workspace")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if got := updated.(Model).chatInput.Value(); got != "q" {
		t.Fatalf("q should be typed into the input, got %q", got)
	}
}
