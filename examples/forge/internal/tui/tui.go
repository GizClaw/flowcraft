// Package tui implements the bubbletea interface for forge: a raid / workspace
// selector and the two-panel chat TUI (chat, workspace).
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/GizClaw/flowcraft/examples/forge/internal/app"
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
				selected := m.items[m.cursor]
				m.selected = &selected
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

// chatMessage is one line group in the chat panel.
type chatMessage struct {
	Role string
	Kind string // "", "tool_call", "tool_result"
	// Label names the speaker when the line came from an agent other than the one
	// the turn drives: a seat reached through delegation.
	Label string
	// Private marks a line only the role acting in its phase could hear.
	Private bool
	Text    string
}

// seatMsg carries one event of a seat, which arrives while the turn is in flight.
type seatMsg struct{ seat app.SeatEvent }

// eventMsg carries one item of the run iterator to the update loop.
type eventMsg struct {
	event agent.Event
	err   error
	done  bool
}

type quitDisarmMsg struct{}

// Model is the two-panel TUI.
type Model struct {
	app           *app.App
	workspacePath string
	messages      []chatMessage
	status        string
	err           string
	running       bool
	// private is whether the seat now speaking is in a phase only its role hears.
	private bool
	// node is the graph node the engine last reported running, which is how a
	// silent node's text stays off the chat.
	node         string
	eventCh      <-chan eventMsg
	width        int
	height       int
	chatInput    textinput.Model
	chatViewport viewport.Model
	chatSpinner  spinner.Model
	quitArmed    bool
	usage        app.Usage
	callNames    map[string]string
}

// NewModel builds a TUI model over an open app. A nil app is accepted so the
// rendering can be tested without a deployment.
func NewModel(a *app.App, workspacePath string) Model {
	chat := textinput.New()
	chat.Placeholder = "message"
	chat.Prompt = "> "
	chat.CharLimit = 4000
	chat.Focus()
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
		chatViewport:  chatViewport,
		chatSpinner:   chatSpinner,
		callNames:     make(map[string]string),
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
			m.chatInput.SetValue("")
			return m, nil
		}
		m.quitArmed = false
		var cmd tea.Cmd
		m.chatViewport, cmd = m.chatViewport.Update(msg)
		if cmd != nil {
			return m, cmd
		}
	case eventMsg:
		if msg.done {
			m.running = false
			m.status = "ready"
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
			return m, m.afterRun()
		}
		m.observe("", msg.event)
		return m, pollCmd(m.eventCh)
	case seatMsg:
		if !m.running {
			return m, nil
		}
		m.private = m.app.Private(msg.seat.Phase)
		m.observe(msg.seat.Label, msg.seat.Event)
		return m, waitSeatCmd(m.app.Events())
	}
	var cmd tea.Cmd
	m.chatInput, cmd = m.chatInput.Update(msg)
	return m, cmd
}

// observe folds one run event into the chat panel and the usage line. A non-empty
// label means the line came from a seat rather than the turn's own agent.
func (m *Model) observe(label string, event agent.Event) {
	switch typed := event.(type) {
	case agent.EventExtension[graph.NodeEvent]:
		m.node = typed.Value.Node
		return
	case agent.Text:
		if label == "" && m.app.Silent(m.node) {
			// The node is the engine's own: its words are a decision, not a line.
			return
		}
		if label != "" {
			m.appendSpeaker(label, typed.Text, m.private)
			return
		}
		m.appendAssistant(typed.Text)
	case agent.ToolCallStart:
		m.callNames[typed.Call.ID] = typed.Call.Name
		// A delegated turn is rendered as the seat speaking, through the seat's own
		// events; the call itself would be the same words twice.
		if typed.Call.Name == "delegate" {
			return
		}
		m.appendTool("tool_call", typed.Call.Name, typed.Call.Arguments.String())
	case agent.ToolResult:
		name := m.callNames[typed.Result.CallID]
		if name == "delegate" {
			return
		}
		if name == "" {
			name = typed.Result.CallID
		}
		m.appendTool("tool_result", name, typed.Result.Content.Text())
	case agent.UsageEvent:
		m.usage.Add(typed.Usage)
	}
}

func (m Model) View() string {
	width, _, bodyHeight, midW := m.panelLayout()
	top := topStyle.Width(width - 2).MaxWidth(width - 2).Render(m.topLine())
	rightW := maxInt(26, width/4)
	mid := panelStyle.Width(midW).Height(bodyHeight).Render(m.chatView(midW, bodyHeight))
	right := panelStyle.Width(rightW).Height(bodyHeight).Render(m.debugView(rightW, bodyHeight))
	return top + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, mid, right) + "\n" +
		helpStyle.Render("enter submit  esc clear  ctrl+c twice quit  ↑/↓ pgup/pgdn scroll")
}

func (m Model) topLine() string {
	info := m.appInfo()
	status := m.status
	if m.quitArmed {
		status = "press ctrl+c again to quit"
	}
	return fmt.Sprintf("Forge TUI  agent=%s  session=%s  status=%s", info.Agent, info.Session, status)
}

func (m Model) appInfo() app.Info {
	if m.app == nil {
		return app.Info{}
	}
	return m.app.Info()
}

func (m Model) chatView(width, height int) string {
	vp := m.chatViewportFor(width, height)
	lines := []string{panelTitleStyle.Render("Chat"), vp.View()}
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
			m.usage.InputTokens, m.usage.OutputTokens, m.usage.Total(),
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
		if msg.Label != "" {
			prefix = "[" + msg.Label + "] "
		}
		if msg.Private {
			prefix = "[私密] " + prefix
		}
		switch msg.Kind {
		case "tool_call":
			prefix = "[工具调用] "
		case "tool_result":
			prefix = "[工具结果] "
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
	info := m.appInfo()
	lines := []string{
		panelTitleStyle.Render("Workspace"),
		"path: " + m.workspacePath,
		"agent: " + info.Agent,
		"engine: " + info.Engine,
		"session: " + info.Session,
		"models:",
	}
	for _, model := range info.Models {
		lines = append(lines, "  "+model)
	}
	lines = append(lines, "tools:")
	for _, item := range info.Tools {
		lines = append(lines, "  "+item)
	}
	return strings.Join(trimLines(wrapLines(lines, width-4), height), "\n")
}

func (m *Model) appendAssistant(text string) {
	// The turn's own words never continue a seat's block: a seat speaks through its
	// own events, and the referee's text after it is a different speaker.
	merge := m.running && len(m.messages) > 0
	if merge {
		last := m.messages[len(m.messages)-1]
		merge = last.Role == "assistant" && last.Label == "" &&
			last.Kind == "" && !last.Private
	}
	if !merge {
		m.messages = append(m.messages, chatMessage{Role: "assistant"})
	}
	last := &m.messages[len(m.messages)-1]
	last.Text += text
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
}

// appendTool records one tool-use block in the chat.
// appendSpeaker writes one seat's line under its label, starting a new block when
// the speaker changes.
func (m *Model) appendSpeaker(label, text string, private bool) {
	merge := len(m.messages) > 0
	if merge {
		last := m.messages[len(m.messages)-1]
		merge = last.Role == "assistant" && last.Label == label &&
			last.Kind == "" && last.Private == private
	}
	if !merge {
		m.messages = append(m.messages, chatMessage{Role: "assistant", Label: label, Private: private})
	}
	last := &m.messages[len(m.messages)-1]
	last.Text += text
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
}

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
	text := strings.TrimSpace(m.chatInput.Value())
	if text == "" || m.running {
		return m, nil
	}
	if text == "/help" {
		m.appendSystem(commandHelp())
		m.chatInput.SetValue("")
		return m, nil
	}
	m.chatInput.SetValue("")
	m.messages = append(m.messages, chatMessage{Role: "user", Text: text})
	m.running = true
	m.status = "running"
	m.err = ""
	m.chatInput.Blur()
	m.usage = app.Usage{}
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
	ch := make(chan eventMsg, 256)
	m.eventCh = ch
	return m, tea.Batch(
		startTurnCmd(m.app, text, ch),
		pollCmd(ch),
		waitSeatCmd(m.app.Events()),
		m.chatSpinner.Tick,
	)
}

// appendSystem writes a local note, not a turn.
func (m *Model) appendSystem(text string) {
	m.messages = append(m.messages, chatMessage{Role: "system", Text: text})
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
}

func commandHelp() string {
	return strings.Join([]string{
		"/help   show this note",
		"",
		"The model and its reasoning effort are deployment settings: they live in",
		"the workspace's runtime layer, so one TUI session cannot switch them.",
	}, "\n")
}

// startTurnCmd streams one turn into ch. It runs on the command goroutine, so the
// update loop stays responsive while the agent works.
func startTurnCmd(a *app.App, text string, ch chan<- eventMsg) tea.Cmd {
	return func() tea.Msg {
		if a == nil {
			ch <- eventMsg{err: fmt.Errorf("no workspace is open")}
			ch <- eventMsg{done: true}
			close(ch)
			return nil
		}
		result, err := a.RunTurn(context.Background(), a.Info().Agent, text, func(event agent.Event) {
			ch <- eventMsg{event: event}
		})
		if err == nil {
			err = app.Failure(result, nil)
		}
		if err != nil {
			ch <- eventMsg{err: err}
		}
		ch <- eventMsg{done: true}
		close(ch)
		return nil
	}
}

// waitSeatCmd waits for the next seat event. It returns nil for a workspace with no
// seat feed, which keeps a nil channel out of the select loop.
func waitSeatCmd(ch <-chan app.SeatEvent) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		seat, ok := <-ch
		if !ok {
			return nil
		}
		return seatMsg{seat: seat}
	}
}

// pollCmd waits for the next event of the running turn.
func pollCmd(ch <-chan eventMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return eventMsg{done: true}
		}
		return msg
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
	rightW := maxInt(26, width/4)
	midW = maxInt(32, width-rightW-6)
	return width, height, bodyHeight, midW
}

func (m *Model) syncChatViewport(width, height int) {
	m.chatViewport = m.chatViewportFor(width, height)
}

func (m *Model) afterRun() tea.Cmd {
	m.chatInput.Focus()
	_, _, bodyHeight, midW := m.panelLayout()
	m.syncChatViewport(midW, bodyHeight)
	return m.chatInput.Focus()
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

var (
	topStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	panelStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	panelTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	helpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	errorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// wrapLine breaks one line at width display columns. Wide runes are measured, not
// counted, so a CJK line wraps where the terminal wraps.
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
