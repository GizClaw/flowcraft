// Package tui implements the bubbletea interface for forge: a raid /
// workspace selector and the three-panel chat TUI (recall, chat,
// workspace).
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/event"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
)

const (
	focusChat = iota
	focusRecall
)

// Item is one selector option.
type Item struct {
	Title string
	Desc  string
	Value string
}

// RunSelector presents a list and returns the selected item.
func RunSelector(title string, items []Item) (Item, bool, error) {
	if len(items) == 0 {
		return Item{}, false, fmt.Errorf("no items available")
	}
	program := tea.NewProgram(newSelectorModel(title, items))
	model, err := program.Run()
	if err != nil {
		return Item{}, false, err
	}
	selector, ok := model.(selectorModel)
	if !ok || selector.canceled || selector.selected == nil {
		return Item{}, false, nil
	}
	return *selector.selected, true, nil
}

type selectorModel struct {
	title    string
	items    []Item
	cursor   int
	selected *Item
	canceled bool
}

func newSelectorModel(title string, items []Item) selectorModel {
	return selectorModel{title: title, items: append([]Item(nil), items...)}
}

func (m selectorModel) Init() tea.Cmd { return nil }

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.canceled = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "enter":
			if len(m.items) > 0 {
				item := m.items[m.cursor]
				m.selected = &item
			}
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m selectorModel) View() string {
	var b strings.Builder
	b.WriteString(selectorTitleStyle.Render(m.title))
	b.WriteString("\n\n")
	if len(m.items) == 0 {
		b.WriteString("No items found.\n")
		return b.String()
	}
	for i, item := range m.items {
		cursor := "  "
		style := selectorItemStyle
		if i == m.cursor {
			cursor = "> "
			style = selectorSelectedStyle
		}
		line := cursor + item.Title
		if item.Desc != "" {
			line += "  " + selectorDescStyle.Render(item.Desc)
		}
		b.WriteString(style.Render(line))
		b.WriteByte('\n')
	}
	b.WriteString("\n" + selectorHelpStyle.Render("up/down select  enter open  ctrl+c quit"))
	return b.String()
}

var (
	selectorTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	selectorItemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	selectorSelectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	selectorDescStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selectorHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

type chatMessage struct {
	Role   string
	NodeID string
	Kind   string // "", "tool_call", "tool_result"
	Text   string
}

type recallResult struct {
	Query string
	Hits  []string
}

type eventMsg struct {
	delta  *agent.StreamDeltaPayload
	nodeID string
	err    error
	done   bool
}

type quitDisarmMsg struct{}

// Model is the three-panel TUI.
type Model struct {
	app           *app.App
	workspacePath string
	messages      []chatMessage
	recall        recallResult
	status        string
	err           string
	running       bool
	eventCh       <-chan eventMsg
	focus         int
	width         int
	height        int
	chatInput     textinput.Model
	recallInput   textinput.Model
	chatViewport  viewport.Model
	chatSpinner   spinner.Model
	quitArmed     bool
	usageBefore   app.UsageSnapshot
	usage         app.UsageSnapshot
	curNodeID     string
}

// NewModel builds a TUI model over an open app.
func NewModel(a *app.App, workspacePath string) Model {
	chat := textinput.New()
	chat.Placeholder = "message"
	chat.Prompt = "> "
	chat.CharLimit = 4000
	chat.Focus()
	recall := textinput.New()
	recall.Placeholder = "recall query"
	recall.Prompt = "? "
	recall.CharLimit = 1000
	chatViewport := viewport.New(0, 0)
	chatViewport.MouseWheelEnabled = true
	chatViewport.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageDown: key.NewBinding(key.WithDisabled()),
		HalfPageUp:   key.NewBinding(key.WithDisabled()),
		Down:         key.NewBinding(key.WithKeys("down")),
		Up:           key.NewBinding(key.WithKeys("up")),
		Left:         key.NewBinding(key.WithDisabled()),
		Right:        key.NewBinding(key.WithDisabled()),
	}
	chatSpinner := spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("39"))),
	)
	return Model{
		app:           a,
		workspacePath: workspacePath,
		status:        "ready",
		chatInput:     chat,
		recallInput:   recall,
		chatViewport:  chatViewport,
		chatSpinner:   chatSpinner,
	}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		_, _, bodyHeight, midW := m.panelLayout()
		m.syncChatViewport(midW, bodyHeight)
		return m, nil
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.chatViewport, cmd = m.chatViewport.Update(msg)
		return m, cmd
	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.chatSpinner, cmd = m.chatSpinner.Update(msg)
		return m, cmd
	case quitDisarmMsg:
		m.quitArmed = false
		return m, nil
	case tea.KeyMsg:
		if isEnter(msg) {
			return m.submitFocusedInput()
		}
		switch msg.String() {
		case "ctrl+c":
			if m.quitArmed {
				return m, tea.Quit
			}
			m.quitArmed = true
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
				return quitDisarmMsg{}
			})
		case "esc":
			m.quitArmed = false
			if m.focus == focusChat {
				m.chatInput.SetValue("")
			} else {
				m.recallInput.SetValue("")
			}
			return m, nil
		case "tab":
			m.quitArmed = false
			if m.focus == focusChat {
				m.focus = focusRecall
				m.chatInput.Blur()
				m.recallInput.Focus()
			} else {
				m.focus = focusChat
				m.recallInput.Blur()
				if m.running {
					m.chatInput.Blur()
				} else {
					m.chatInput.Focus()
				}
			}
			return m, nil
		}
		m.quitArmed = false
		if m.focus == focusChat {
			var cmd tea.Cmd
			m.chatViewport, cmd = m.chatViewport.Update(msg)
			if cmd != nil {
				return m, cmd
			}
		}
	case eventMsg:
		if msg.done {
			m.running = false
			m.status = "ready"
			m.usage = m.app.Usage().Since(m.usageBefore)
			if msg.err != nil {
				m.err = msg.err.Error()
				m.status = "error"
			} else {
				m.err = ""
			}
			return m, m.afterRun()
		}
		if msg.err != nil {
			m.running = false
			m.err = msg.err.Error()
			m.status = "error"
			m.usage = m.app.Usage().Since(m.usageBefore)
			return m, m.afterRun()
		}
		if msg.delta != nil {
			switch msg.delta.Type {
			case agent.StreamDeltaToken:
				m.appendAssistant(msg.delta.Content, msg.nodeID)
			case agent.StreamDeltaToolCall:
				m.appendTool("tool_call", msg.delta.Name, fmt.Sprint(msg.delta.Arguments))
			case agent.StreamDeltaToolResult:
				m.appendTool("tool_result", msg.delta.Name, msg.delta.Content)
			}
		}
		return m, pollCmd(m.eventCh)
	case recallMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			m.status = "error"
			return m, nil
		}
		m.recall = msg.result
		if len(msg.result.Hits) == 0 {
			m.status = "recall: no hits"
		} else {
			m.status = fmt.Sprintf("recall: %d hits", len(msg.result.Hits))
		}
		return m, nil
	}
	var cmd tea.Cmd
	if m.focus == focusRecall {
		m.recallInput, cmd = m.recallInput.Update(msg)
	} else {
		m.chatInput, cmd = m.chatInput.Update(msg)
	}
	return m, cmd
}

func (m Model) View() string {
	width, _, bodyHeight, midW := m.panelLayout()
	top := topStyle.Width(width - 2).MaxWidth(width - 2).Render(m.topLine())
	leftW := maxInt(24, width/4)
	rightW := maxInt(26, width/4)
	left := panelStyle.Width(leftW).Height(bodyHeight).Render(m.recallView(leftW, bodyHeight))
	mid := panelStyle.Width(midW).Height(bodyHeight).Render(m.chatView(midW, bodyHeight))
	right := panelStyle.Width(rightW).Height(bodyHeight).Render(m.debugView(rightW, bodyHeight))
	return top + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right) + "\n" +
		helpStyle.Render("tab focus  enter submit  esc clear  ctrl+c twice quit  ↑/↓ pgup/pgdn scroll")
}

func (m Model) topLine() string {
	info := m.app.Info()
	status := m.status
	if m.quitArmed {
		status = "press ctrl+c again to quit"
	}
	return fmt.Sprintf("Forge TUI  agent=%s  context=%s  status=%s",
		info.AgentName, info.ContextID, status)
}

func (m Model) recallView(width, height int) string {
	title := "Recall"
	if m.focus == focusRecall {
		title += " *"
	}
	lines := []string{panelTitleStyle.Render(title), m.recallInput.View(), ""}
	if m.recall.Query == "" {
		lines = append(lines, "Enter a query.")
	} else if len(m.recall.Hits) == 0 {
		lines = append(lines, fmt.Sprintf("No recall hits for %q.", m.recall.Query))
	} else {
		lines = append(lines, fmt.Sprintf("%d hits for %q:", len(m.recall.Hits), m.recall.Query), "")
		for _, hit := range m.recall.Hits {
			lines = append(lines, wrapLine(hit, width-4)...)
		}
	}
	return strings.Join(trimLines(lines, height), "\n")
}

func (m Model) chatView(width, height int) string {
	title := "Chat"
	if m.focus == focusChat {
		title += " *"
	}
	vp := m.chatViewportFor(width, height)
	lines := []string{panelTitleStyle.Render(title), vp.View()}
	if m.err != "" {
		lines = append(lines, errorStyle.Render("error: "+m.err))
	}
	input := m.chatInput.View()
	if m.running {
		input = m.chatSpinner.View() + " " + helpStyle.Render("agent running…")
	}
	lines = append(lines, "", input)
	lines = append(lines, m.usageLines(width)...)
	return strings.Join(lines, "\n")
}

func (m Model) usageLines(width int) []string {
	var text string
	if m.usage.Calls <= 0 {
		text = "usage: -"
	} else {
		text = fmt.Sprintf("usage in %d out %d total %d reason %d cache_r %d cache_w %d calls %d",
			m.usage.InputTokens, m.usage.OutputTokens, m.usage.TotalTokens,
			m.usage.ReasoningTokens, m.usage.CacheReadTokens, m.usage.CacheWriteTokens,
			m.usage.Calls)
	}
	lines := wrapLine(text, width-4)
	for i := range lines {
		lines[i] = helpStyle.Render(lines[i])
	}
	return lines
}

func (m Model) chatContent(width int) string {
	var b strings.Builder
	for _, msg := range m.messages {
		text := strings.TrimSpace(msg.Text)
		if text == "" && msg.Role == "assistant" && m.running {
			text = "..."
		}
		prefix := msg.Role + ": "
		switch msg.Kind {
		case "tool_call":
			prefix = "[工具调用] "
		case "tool_result":
			prefix = "[工具结果] "
		default:
			if msg.Role == "assistant" {
				if m.app != nil {
					if label := m.app.SpeakerLabel(msg.NodeID); label != "" {
						prefix = "[" + label + "] "
					}
				}
			}
		}
		for _, line := range wrapLine(prefix+text, width-4) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m Model) debugView(width, height int) string {
	info := m.app.Info()
	lines := []string{
		panelTitleStyle.Render("Workspace"),
		"path: " + m.workspacePath,
		"agent: " + info.AgentName,
		"generate: " + info.GenerateModel,
		"context: " + info.ContextID,
		"",
		panelTitleStyle.Render("Memory"),
		fmt.Sprintf("enabled: %t", info.MemoryEnabled),
		"top_k: " + fmt.Sprint(info.MemoryTopK),
	}
	return strings.Join(trimLines(wrapLines(lines, width-4), height), "\n")
}

func (m *Model) appendAssistant(text, nodeID string) {
	if len(m.messages) == 0 ||
		m.messages[len(m.messages)-1].Role != "assistant" ||
		!m.running ||
		(nodeID != "" && nodeID != m.curNodeID) {
		m.messages = append(m.messages, chatMessage{
			Role:   "assistant",
			NodeID: nodeID,
		})
	}
	if nodeID != "" {
		m.curNodeID = nodeID
	}
	last := &m.messages[len(m.messages)-1]
	last.Text += text
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
}

// appendTool records one tool-use block in the chat.
func (m *Model) appendTool(kind, name, detail string) {
	text := name
	if detail != "" {
		if kind == "tool_result" {
			text += ": " + detail
		} else {
			text += " " + detail
		}
	}
	m.messages = append(m.messages, chatMessage{Role: "tool", Kind: kind, Text: text})
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
}

func (m Model) submitFocusedInput() (tea.Model, tea.Cmd) {
	m.quitArmed = false
	if m.focus == focusRecall {
		query := strings.TrimSpace(m.recallInput.Value())
		if query == "" {
			return m, nil
		}
		m.recall.Query = query
		m.status = "recalling"
		return m, recallCmd(m.app, query)
	}
	text := strings.TrimSpace(m.chatInput.Value())
	if text == "" || m.running {
		return m, nil
	}
	m.chatInput.SetValue("")
	m.messages = append(m.messages, chatMessage{Role: "user", Text: text})
	m.running = true
	m.status = "running"
	m.err = ""
	m.chatInput.Blur()
	m.usageBefore = m.app.Usage()
	m.curNodeID = ""
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
	ch := make(chan eventMsg, 256)
	m.eventCh = ch
	return m, tea.Batch(startRoundCmd(m.app, text, ch), pollCmd(ch), m.chatSpinner.Tick)
}

func (m Model) panelLayout() (width, height, bodyHeight, midW int) {
	width, height = m.width, m.height
	if width <= 0 {
		width = 120
	}
	if height <= 0 {
		height = 32
	}
	bodyHeight = height - 4
	if bodyHeight < 12 {
		bodyHeight = 12
	}
	leftW := maxInt(24, width/4)
	rightW := maxInt(26, width/4)
	midW = maxInt(32, width-leftW-rightW-6)
	return width, height, bodyHeight, midW
}

func (m *Model) syncChatViewport(width, height int) {
	m.chatViewport = m.chatViewportFor(width, height)
}

func (m *Model) afterRun() tea.Cmd {
	var cmds []tea.Cmd
	if m.focus == focusChat {
		cmds = append(cmds, m.chatInput.Focus())
	}
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
	return tea.Batch(cmds...)
}

func (m Model) chatViewportFor(width, height int) viewport.Model {
	vp := m.chatViewport
	vp.Width = maxInt(1, width-4)
	vp.Height = maxInt(1, height-3-len(m.usageLines(width)))
	if m.err != "" {
		vp.Height = maxInt(1, vp.Height-1)
	}
	follow := vp.AtBottom()
	vp.SetContent(m.chatContent(width))
	if follow {
		vp.GotoBottom()
	}
	return vp
}

func startRoundCmd(a *app.App, text string, ch chan<- eventMsg) tea.Cmd {
	return func() tea.Msg {
		sink := session.SinkSpec{
			ID: "tui",
			Sink: agent.StreamSinkFunc(func(_ context.Context, env event.Envelope, delta agent.StreamDeltaPayload) error {
				d := delta
				ch <- eventMsg{delta: &d, nodeID: env.NodeID()}
				return nil
			}),
		}
		_, err := a.RunTurn(context.Background(), text, sink)
		if err != nil {
			ch <- eventMsg{err: err}
		}
		ch <- eventMsg{done: true}
		close(ch)
		return eventMsg{}
	}
}

func pollCmd(ch <-chan eventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return eventMsg{done: true}
		}
		return msg
	}
}

type recallMsg struct {
	result recallResult
	err    error
}

func recallCmd(a *app.App, query string) tea.Cmd {
	return func() tea.Msg {
		if a.Memory() == nil {
			return recallMsg{result: recallResult{Query: query}}
		}
		info := a.Info()
		result, err := a.Memory().Context(context.Background(), sdkmemory.ContextRequest{
			Scope:  sdkmemory.Scope{RuntimeID: info.MemoryScope.RuntimeID, UserID: info.MemoryScope.UserID, AgentID: info.MemoryScope.AgentID},
			Query:  query,
			Budget: sdkmemory.Budget{MaxItems: info.MemoryTopK},
		})
		if err != nil {
			return recallMsg{err: err}
		}
		hits := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			hits = append(hits, strings.TrimSpace(item.Content.Text()))
		}
		return recallMsg{result: recallResult{Query: query, Hits: hits}}
	}
}

func isEnter(msg tea.KeyMsg) bool {
	if msg.Alt {
		return false
	}
	if msg.Type == tea.KeyEnter || msg.Type == tea.KeyCtrlJ || msg.Type == tea.KeyCtrlM || msg.String() == "enter" {
		return true
	}
	return msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && (msg.Runes[0] == '\r' || msg.Runes[0] == '\n')
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func wrapLine(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var out []string
	var cur strings.Builder
	curW := 0
	for _, r := range text {
		w := runewidth.RuneWidth(r)
		if w < 0 {
			w = 0
		}
		if curW > 0 && curW+w > width {
			out = append(out, cur.String())
			cur.Reset()
			curW = 0
		}
		cur.WriteRune(r)
		curW += w
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

func wrapLines(lines []string, width int) []string {
	var out []string
	for _, line := range lines {
		out = append(out, wrapLine(line, width)...)
	}
	return out
}

func trimLines(lines []string, height int) []string {
	if len(lines) <= height {
		return lines
	}
	return lines[len(lines)-height:]
}

var (
	topStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	panelStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	panelTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	helpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)
