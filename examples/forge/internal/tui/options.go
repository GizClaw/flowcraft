package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GizClaw/flowcraft/core/inference"
)

// Slash commands that configure this TUI process. They never reach the
// agent: a command edits local state and the next turn carries it as engine
// inputs.
const (
	commandModel = "/model"
	commandThink = "/think"
)

// runCommand handles one slash command. It returns the updated model and a
// flag telling the caller whether input was consumed. Only this TUI's own
// commands are intercepted: scenario-level directives such as /start and
// /next are user text the agent reads, so anything unknown passes through.
func (m Model) runCommand(text string) (Model, tea.Cmd, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return m, nil, false
	}
	switch fields[0] {
	case commandModel:
		return m.runModelCommand(fields[1:])
	case commandThink:
		return m.runThinkCommand(fields[1:])
	case "/help":
		m.appendSystem(commandHelp())
		return m, nil, true
	default:
		return m, nil, false
	}
}

func commandHelp() string {
	return strings.Join([]string{
		"/model          choose the model for the next turns (auto or a specific target)",
		"/model auto     let the routing policy pick the model",
		"/model <target> use one target, e.g. glm/glm-5.3-flash",
		"/think          choose the reasoning effort (auto or a canonical level)",
		"/think auto     leave the effort to the provider default",
		"/think <level>  " + strings.Join(thinkLevels(), "/"),
	}, "\n")
}

// runModelCommand opens the model picker, or applies an explicit argument.
func (m Model) runModelCommand(args []string) (Model, tea.Cmd, bool) {
	if len(args) == 0 {
		items := []Item{{Title: autoLabel, Desc: "routing policy default", Value: ""}}
		for _, model := range m.app.Models() {
			items = append(items, Item{Title: model, Value: model})
		}
		picker := newSelectorModel("Select model", items)
		m.picker = &picker
		m.pickerFor = pickerModel
		return m, nil, true
	}
	choice := args[0]
	if choice == autoLabel || choice == "" {
		m.model = ""
		m.appendSystem("model: auto")
		return m, nil, true
	}
	for _, model := range m.app.Models() {
		if model == choice {
			m.model = choice
			m.appendSystem("model: " + choice)
			return m, nil, true
		}
	}
	m.appendSystem(fmt.Sprintf(
		"unknown model %q\navailable: %s",
		choice,
		strings.Join(append([]string{autoLabel}, m.app.Models()...), ", "),
	))
	return m, nil, true
}

// runThinkCommand opens the effort picker, or applies an explicit argument.
func (m Model) runThinkCommand(args []string) (Model, tea.Cmd, bool) {
	if len(args) == 0 {
		items := []Item{{Title: autoLabel, Desc: "provider default", Value: ""}}
		for _, level := range thinkLevels() {
			items = append(items, Item{Title: level, Value: level})
		}
		picker := newSelectorModel("Select reasoning effort", items)
		m.picker = &picker
		m.pickerFor = pickerThink
		return m, nil, true
	}
	choice := args[0]
	if choice == autoLabel || choice == "" {
		m.think = ""
		m.appendSystem("think: auto")
		return m, nil, true
	}
	for _, level := range thinkLevels() {
		if level == choice {
			m.think = level
			m.appendSystem("think: " + level)
			return m, nil, true
		}
	}
	m.appendSystem(fmt.Sprintf(
		"unknown reasoning level %q\navailable: %s",
		choice,
		strings.Join(append([]string{autoLabel}, thinkLevels()...), ", "),
	))
	return m, nil, true
}

// updatePicker routes keys to the open option list. The embedded picker
// cannot reuse selectorModel.Update: that one quits the whole program on
// enter or escape, which here would end the TUI instead of the picker.
func (m Model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	picker := m.picker
	switch msg.String() {
	case "esc", "ctrl+c":
		m.picker = nil
		m.pickerFor = ""
		return m, nil
	case "up", "k":
		if picker.cursor > 0 {
			picker.cursor--
		}
		return m, nil
	case "down", "j":
		if picker.cursor < len(picker.items)-1 {
			picker.cursor++
		}
		return m, nil
	case "enter":
		if len(picker.items) > 0 {
			m.applyPick(picker.items[picker.cursor].Value)
		}
		m.picker = nil
		m.pickerFor = ""
		return m, nil
	}
	return m, nil
}

func (m *Model) applyPick(value string) {
	switch m.pickerFor {
	case pickerModel:
		m.model = value
		m.appendSystem("model: " + autoOr(value))
	case pickerThink:
		m.think = value
		m.appendSystem("think: " + autoOr(value))
	}
}

// appendSystem writes a host note into the chat transcript. It is not an
// agent turn: no inference runs and the message never reaches the model.
func (m *Model) appendSystem(text string) {
	m.messages = append(m.messages, chatMessage{Role: "system", Text: text})
}

// selection renders the current preferences for the status line.
func (m Model) selection() string {
	return fmt.Sprintf("model=%s think=%s", autoOr(m.model), autoOr(m.think))
}

func autoOr(value string) string {
	if value == "" {
		return autoLabel
	}
	return value
}

const (
	autoLabel   = "auto"
	pickerModel = "model"
	pickerThink = "think"
)

// thinkLevels lists the canonical reasoning levels every provider folds onto
// its own ladder; the driver reports any fold on the compile ledger.
func thinkLevels() []string {
	return []string{
		string(inference.ReasoningMinimal),
		string(inference.ReasoningLow),
		string(inference.ReasoningMedium),
		string(inference.ReasoningHigh),
		string(inference.ReasoningXHigh),
	}
}
