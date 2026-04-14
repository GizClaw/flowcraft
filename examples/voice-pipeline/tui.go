package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

type chatLine struct {
	role string
	text string
}

type (
	appendLineMsg    chatLine
	updatePartialMsg string
	clearPartialMsg  struct{}
	appendAIDeltaMsg string
	flushAIStreamMsg struct{}
)

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	statStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			Padding(0, 1)

	userStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("213")).
			Bold(true)

	aiStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("123"))

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("245")).
			Italic(true)

	partialStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))

	inputPromptStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("213")).
				Bold(true)

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
)

type voiceInfo struct {
	ID   string
	Name string
	Lang string
}

type tuiModel struct {
	viewport viewport.Model
	input    textinput.Model
	lines    []chatLine
	partial  string
	aiStream string
	width    int
	height   int
	ready    bool
	onSubmit func(string)
	onReset  func()
	voices   []voiceInfo
	voiceID  *string
}

func newTUIModel(onSubmit func(string), onReset func(), voices []voiceInfo, voiceID *string) tuiModel {
	ti := textinput.New()
	ti.Placeholder = "Type a message (or speak into the mic)..."
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "❯ "
	ti.PromptStyle = inputPromptStyle

	return tuiModel{
		input:    ti,
		onSubmit: onSubmit,
		onReset:  onReset,
		voices:   voices,
		voiceID:  voiceID,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg), nil

	case appendLineMsg:
		m.partial = ""
		if m.aiStream != "" {
			m.lines = append(m.lines, chatLine{role: "ai", text: m.aiStream})
			m.aiStream = ""
		}
		m.lines = append(m.lines, chatLine(msg))
		m.refreshViewport()
		return m, nil

	case updatePartialMsg:
		m.partial = string(msg)
		m.refreshViewport()
		return m, nil

	case clearPartialMsg:
		m.partial = ""
		m.refreshViewport()
		return m, nil

	case appendAIDeltaMsg:
		m.aiStream += string(msg)
		m.refreshViewport()
		return m, nil

	case flushAIStreamMsg:
		if m.aiStream != "" {
			m.lines = append(m.lines, chatLine{role: "ai", text: m.aiStream})
			m.aiStream = ""
		}
		m.refreshViewport()
		return m, nil
	}

	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch keyMsg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit

		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()

			if strings.HasPrefix(text, "/") {
				return m.handleCommand(text)
			}
			if m.onSubmit != nil {
				m.onSubmit(text)
			}
			return m, nil
		}
	}

	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	if m.ready {
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m tuiModel) handleCommand(text string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/voice":
		if len(parts) > 1 {
			id := parts[1]
			found := false
			for _, v := range m.voices {
				if v.ID == id {
					found = true
					break
				}
			}
			if found && m.voiceID != nil {
				*m.voiceID = id
				m.lines = append(m.lines, chatLine{role: "status", text: fmt.Sprintf("Voice switched: %s", id)})
			} else {
				m.lines = append(m.lines, chatLine{role: "status", text: fmt.Sprintf("Unknown voice %q, type /voice to list available voices", id)})
			}
			m.refreshViewport()
			return m, nil
		}
		cur := ""
		if m.voiceID != nil {
			cur = *m.voiceID
		}
		m.lines = append(m.lines, chatLine{role: "status", text: fmt.Sprintf("Current voice: %s", cur)})
		for _, v := range m.voices {
			marker := "  "
			if v.ID == cur {
				marker = "▸ "
			}
			m.lines = append(m.lines, chatLine{role: "status", text: fmt.Sprintf("  %s%-28s %s [%s]", marker, v.ID, v.Name, v.Lang)})
		}
		m.lines = append(m.lines, chatLine{role: "status", text: "Usage: /voice <id>"})
		m.refreshViewport()
		return m, nil

	case "/reset":
		if m.onReset != nil {
			m.onReset()
		}
		m.lines = nil
		m.partial = ""
		m.aiStream = ""
		m.lines = append(m.lines, chatLine{role: "status", text: "Session reset and screen cleared (no persistent memory)"})
		m.refreshViewport()
		return m, nil

	default:
		m.lines = append(m.lines, chatLine{role: "status", text: fmt.Sprintf("Unknown command: %s (available: /voice, /reset)", cmd)})
		m.refreshViewport()
		return m, nil
	}
}

func (m tuiModel) handleResize(msg tea.WindowSizeMsg) tuiModel {
	m.width = msg.Width
	m.height = msg.Height
	vpHeight := m.chatViewportHeight()
	if !m.ready {
		m.viewport = viewport.New(m.width-4, vpHeight)
		m.viewport.SetContent(m.renderChat())
		m.ready = true
	} else {
		m.viewport.Width = m.width - 4
		m.viewport.Height = vpHeight
		m.viewport.SetContent(m.renderChat())
	}
	m.input.Width = m.width - 6
	return m
}

const (
	headerLines = 2
	inputLines  = 2
	helpLines   = 1
	chrome      = headerLines + inputLines + helpLines + 4
)

func (m tuiModel) chatViewportHeight() int {
	h := m.height - chrome
	if h < 3 {
		h = 3
	}
	return h
}

func (m *tuiModel) refreshViewport() {
	if !m.ready {
		return
	}
	m.viewport.SetContent(m.renderChat())
	m.viewport.GotoBottom()
}

func (m tuiModel) View() string {
	if !m.ready {
		return "initializing..."
	}
	header := m.renderHeader()
	chat := borderStyle.Width(m.width - 2).Render(m.viewport.View())
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		chat,
		"  "+m.input.View(),
		helpStyle.Render("  Enter send · /voice switch voice · /reset clear session · Ctrl+C quit"),
	)
}

func (m tuiModel) renderHeader() string {
	title := headerStyle.Render(" Voice pipeline (example) ")
	cur := ""
	if m.voiceID != nil {
		cur = *m.voiceID
	}
	stats := statStyle.Render("TTS voice: " + cur)
	return title + "  " + stats
}

func (m tuiModel) renderChat() string {
	if len(m.lines) == 0 && m.partial == "" && m.aiStream == "" {
		return statusStyle.Render("  Waiting for voice or text input...")
	}
	contentWidth := m.contentWidth()
	var sb strings.Builder
	for _, line := range m.lines {
		sb.WriteString(m.formatLine(line, contentWidth))
		sb.WriteByte('\n')
	}
	if m.aiStream != "" {
		prefix := "  AI  ❯ "
		pw := runewidth.StringWidth(prefix)
		sb.WriteString(aiStyle.Render(prefix) + softWrap(m.aiStream, contentWidth, pw))
		sb.WriteByte('\n')
	}
	if m.partial != "" {
		prefix := "  🔄 "
		pw := runewidth.StringWidth(prefix)
		sb.WriteString(partialStyle.Render(prefix + softWrap(m.partial, contentWidth, pw)))
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (m tuiModel) contentWidth() int {
	w := m.width - 6
	if w < 20 {
		w = 20
	}
	return w
}

func (m tuiModel) formatLine(line chatLine, maxWidth int) string {
	switch line.role {
	case "user":
		prefix := "  You ❯ "
		pw := runewidth.StringWidth(prefix)
		return userStyle.Render(prefix) + softWrap(line.text, maxWidth, pw)
	case "ai":
		prefix := "  AI  ❯ "
		pw := runewidth.StringWidth(prefix)
		return aiStyle.Render(prefix) + softWrap(line.text, maxWidth, pw)
	case "status":
		prefix := "  "
		pw := runewidth.StringWidth(prefix)
		return statusStyle.Render(prefix + softWrap(line.text, maxWidth, pw))
	default:
		return "  " + softWrap(line.text, maxWidth, 2)
	}
}

func softWrap(text string, maxWidth, prefixCols int) string {
	lineWidth := maxWidth - prefixCols
	if lineWidth < 10 {
		lineWidth = 10
	}
	pad := strings.Repeat(" ", prefixCols)
	paragraphs := strings.Split(text, "\n")
	var sb strings.Builder
	for i, para := range paragraphs {
		if i > 0 {
			sb.WriteByte('\n')
			sb.WriteString(pad)
		}
		col := 0
		for _, r := range para {
			rw := runewidth.RuneWidth(r)
			if col+rw > lineWidth && col > 0 {
				sb.WriteByte('\n')
				sb.WriteString(pad)
				col = 0
			}
			sb.WriteRune(r)
			col += rw
		}
	}
	return sb.String()
}
