package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/sdkx/claw"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	tuiFocusChat = iota
	tuiFocusRecall
)

const clawHistoryXMLPartType = "text/xml"

type tuiChatMessage struct {
	Role   string
	Text   string
	NodeID string
}

type tuiRecallHit struct {
	Content string  `json:"content"`
	Kind    string  `json:"kind"`
	Score   float64 `json:"score"`
}

type tuiRecallResult struct {
	Enabled bool
	Query   string
	Hits    []tuiRecallHit
}

type tuiModel struct {
	app           *claw.Claw
	workspacePath string
	cfg           claw.Config
	messages      []tuiChatMessage
	recall        tuiRecallResult
	status        string
	err           string
	running       bool
	autoStart     bool
	activeResp    *claw.Response
	activeIndex   int
	focus         int
	width         int
	height        int
	chatInput     textinput.Model
	recallInput   textinput.Model
}

func newTUIModel(app *claw.Claw, workspacePath string) tuiModel {
	cfg := claw.Config{}
	if app != nil {
		cfg = app.Config()
	}
	chat := textinput.New()
	chat.Placeholder = "message"
	chat.Prompt = "> "
	chat.CharLimit = 4000
	chat.Focus()
	recall := textinput.New()
	recall.Placeholder = "recall query"
	recall.Prompt = "? "
	recall.CharLimit = 1000
	m := tuiModel{
		app:           app,
		workspacePath: workspacePath,
		cfg:           cfg,
		status:        "ready",
		activeIndex:   -1,
		chatInput:     chat,
		recallInput:   recall,
	}
	if app != nil {
		m.messages = loadTUIHistory(app, cfg.Conversation.ContextID)
	}
	if app != nil && shouldTUIAutoStart(cfg, len(m.messages)) {
		m.messages = append(m.messages, tuiChatMessage{Role: "assistant"})
		m.activeIndex = len(m.messages) - 1
		m.running = true
		m.autoStart = true
		m.status = "running"
	}
	return m
}

func shouldTUIAutoStart(cfg claw.Config, messageCount int) bool {
	return messageCount == 0 && strings.EqualFold(strings.TrimSpace(cfg.Conversation.Starts), "self")
}

func (m tuiModel) Init() tea.Cmd {
	if m.autoStart {
		return tuiStartRoundCmd(m.app, "")
	}
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		if isTUISubmitKey(msg) {
			return m.submitFocusedInput()
		}
		switch msg.String() {
		case "ctrl+c":
			return m, m.quitCmd()
		case "tab":
			m.toggleFocus()
			return m, nil
		case "esc":
			return m, m.quitCmd()
		case "q":
			return m, m.quitCmd()
		}
	case tuiRoundStartedMsg:
		if msg.err != nil {
			m.running = false
			m.activeResp = nil
			m.err = msg.err.Error()
			m.status = "error"
			return m, nil
		}
		m.activeResp = msg.resp
		return m, tuiReadRoundCmd(msg.resp)
	case tuiRoundEventMsg:
		if errors.Is(msg.err, io.EOF) {
			m.running = false
			m.activeResp = nil
			m.status = "ready"
			m.activeIndex = -1
			return m, nil
		}
		if msg.err != nil {
			m.running = false
			m.activeResp = nil
			m.err = msg.err.Error()
			m.status = "error"
			m.activeIndex = -1
			return m, nil
		}
		m.applyRoundEvent(msg.ev)
		return m, tuiReadRoundCmd(msg.resp)
	case tuiRecallMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			m.status = "error"
			return m, nil
		}
		m.recall = msg.result
		if !msg.result.Enabled {
			m.status = "recall disabled"
		} else {
			m.status = fmt.Sprintf("recall: %d hits", len(msg.result.Hits))
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.focus == tuiFocusRecall {
		m.recallInput, cmd = m.recallInput.Update(msg)
	} else {
		m.chatInput, cmd = m.chatInput.Update(msg)
	}
	return m, cmd
}

func isTUISubmitKey(msg tea.KeyMsg) bool {
	if msg.Alt {
		return false
	}
	if msg.Type == tea.KeyEnter || msg.Type == tea.KeyCtrlJ || msg.Type == tea.KeyCtrlM || msg.String() == "enter" {
		return true
	}
	return msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && (msg.Runes[0] == '\r' || msg.Runes[0] == '\n')
}

func (m tuiModel) submitFocusedInput() (tea.Model, tea.Cmd) {
	if m.focus == tuiFocusRecall {
		query := strings.TrimSpace(m.recallInput.Value())
		if query == "" {
			return m, nil
		}
		m.recall.Query = query
		m.status = "recalling"
		return m, tuiRecallCmd(m.app, query)
	}
	text := strings.TrimSpace(m.chatInput.Value())
	if text == "" {
		return m, nil
	}
	if m.running {
		if strings.Contains(m.status, "extracting") {
			m.status = "extracting; wait or q to stop"
		} else {
			m.status = "running; wait or q to stop"
		}
		return m, nil
	}
	m.chatInput.SetValue("")
	return m.beginRound(text, true)
}

func (m tuiModel) quitCmd() tea.Cmd {
	if m.running && m.activeResp != nil {
		_ = m.activeResp.Interrupt(false)
	}
	return tea.Quit
}

func (m tuiModel) beginRound(text string, showUser bool) (tuiModel, tea.Cmd) {
	if showUser {
		m.messages = append(m.messages, tuiChatMessage{Role: "user", Text: text})
	}
	m.messages = append(m.messages, tuiChatMessage{Role: "assistant"})
	m.activeIndex = len(m.messages) - 1
	m.running = true
	m.status = "running"
	return m, tuiStartRoundCmd(m.app, text)
}

func (m *tuiModel) toggleFocus() {
	if m.focus == tuiFocusChat {
		m.focus = tuiFocusRecall
		m.chatInput.Blur()
		m.recallInput.Focus()
		return
	}
	m.focus = tuiFocusChat
	m.recallInput.Blur()
	m.chatInput.Focus()
}

func (m *tuiModel) applyRoundEvent(ev claw.Event) {
	switch ev.Type {
	case claw.EventToken:
		nodeID := strings.TrimSpace(ev.NodeID)
		if m.activeIndex < 0 || m.activeIndex >= len(m.messages) || m.messages[m.activeIndex].Role != "assistant" {
			m.messages = append(m.messages, tuiChatMessage{Role: "assistant", NodeID: nodeID})
			m.activeIndex = len(m.messages) - 1
		}
		current := &m.messages[m.activeIndex]
		if nodeID != "" && current.NodeID != "" && current.NodeID != nodeID {
			m.messages = append(m.messages, tuiChatMessage{Role: "assistant", NodeID: nodeID})
			m.activeIndex = len(m.messages) - 1
			current = &m.messages[m.activeIndex]
		}
		if nodeID != "" && current.NodeID == "" {
			current.NodeID = nodeID
		}
		current.Text += ev.Content
	case claw.EventResult:
		if ev.Result != nil {
			if text := latestAssistantText(ev.Result.Messages); text != "" && m.activeIndex >= 0 && m.activeIndex < len(m.messages) && strings.TrimSpace(m.messages[m.activeIndex].Text) == "" {
				m.messages[m.activeIndex].Text = text
			}
		}
	case claw.EventStatus:
		if strings.TrimSpace(ev.Content) != "" {
			m.status = strings.TrimSpace(ev.Content)
		}
	case claw.EventError:
		m.err = ev.Err
		m.status = "error"
	}
}

func (m tuiModel) View() string {
	width := m.width
	if width <= 0 {
		width = 120
	}
	height := m.height
	if height <= 0 {
		height = 32
	}
	top := tuiTopStyle.Width(width - 2).Render(m.topLine())
	bodyHeight := height - 4
	if bodyHeight < 12 {
		bodyHeight = 12
	}
	leftW := maxInt(24, width/4)
	rightW := maxInt(26, width/4)
	midW := width - leftW - rightW - 6
	if midW < 32 {
		midW = 32
	}
	left := tuiPanelStyle.Width(leftW).Height(bodyHeight).Render(m.recallView(leftW, bodyHeight))
	mid := tuiPanelStyle.Width(midW).Height(bodyHeight).Render(m.chatView(midW, bodyHeight))
	right := tuiPanelStyle.Width(rightW).Height(bodyHeight).Render(m.debugView(rightW, bodyHeight))
	return top + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, left, mid, right) + "\n" + tuiHelpStyle.Render("tab focus  enter submit  q/esc quit")
}

func (m tuiModel) topLine() string {
	agent := m.cfg.Agent.Name
	if strings.TrimSpace(agent) == "" {
		agent = m.cfg.Agent.ID
	}
	if strings.TrimSpace(agent) == "" {
		agent = "Claw"
	}
	contextID := m.cfg.Conversation.ContextID
	if contextID == "" {
		contextID = "__default__"
	}
	return fmt.Sprintf("Claw TUI  agent=%s  context=%s  status=%s", agent, contextID, m.status)
}

func (m tuiModel) recallView(width, height int) string {
	var lines []string
	title := "Recall"
	if m.focus == tuiFocusRecall {
		title += " *"
	}
	lines = append(lines, tuiPanelTitleStyle.Render(title), m.recallInput.View(), "")
	if strings.TrimSpace(m.recall.Query) == "" {
		lines = append(lines, "Enter a query.")
	} else if m.status == "recalling" {
		lines = append(lines, fmt.Sprintf("Searching for %q...", m.recall.Query))
	} else if !m.recall.Enabled {
		lines = append(lines, "Memory disabled.")
	} else if len(m.recall.Hits) == 0 {
		lines = append(lines, fmt.Sprintf("No recall hits for %q.", m.recall.Query))
	} else {
		lines = append(lines, fmt.Sprintf("%d hits for %q:", len(m.recall.Hits), m.recall.Query), "")
		for _, hit := range m.recall.Hits {
			line := strings.TrimSpace(hit.Content)
			if hit.Kind != "" {
				line = "[" + hit.Kind + "] " + line
			}
			lines = append(lines, wrapLine(line, width-4)...)
		}
	}
	return trimLines(lines, height)
}

func (m tuiModel) chatView(width, height int) string {
	var lines []string
	title := "Chat"
	if m.focus == tuiFocusChat {
		title += " *"
	}
	lines = append(lines, tuiPanelTitleStyle.Render(title))
	msgHeight := height - 5
	for _, msg := range m.messages {
		text := strings.TrimSpace(msg.Text)
		if text == "" && msg.Role == "assistant" && m.running {
			text = "..."
		}
		if msg.Role == "user" {
			lines = append(lines, wrapLine("user: "+text, width-4)...)
		} else {
			lines = append(lines, wrapLine(tuiMessageLabel(msg)+" "+text, width-4)...)
		}
		lines = append(lines, "")
	}
	if len(lines) > msgHeight {
		lines = append([]string{lines[0]}, lines[len(lines)-msgHeight:]...)
	}
	if m.err != "" {
		lines = append(lines, tuiErrorStyle.Render("error: "+m.err))
	}
	lines = append(lines, "", m.chatInput.View())
	return trimLines(lines, height)
}

func tuiMessageLabel(msg tuiChatMessage) string {
	if msg.Role == "user" {
		return "user"
	}
	if strings.TrimSpace(msg.NodeID) != "" {
		return "<" + msg.NodeID + ">"
	}
	return "<node>"
}

func (m tuiModel) debugView(width, height int) string {
	cfg := m.cfg
	lines := []string{
		tuiPanelTitleStyle.Render("Workspace"),
		"path: " + m.workspacePath,
		"agent: " + firstNonEmpty(cfg.Agent.ID, cfg.Agent.Name),
		"chat_model: " + cfg.Models.Chat,
		"context: " + firstNonEmpty(cfg.Conversation.ContextID, "__default__"),
		"",
		tuiPanelTitleStyle.Render("Memory"),
		fmt.Sprintf("enabled: %t", cfg.Memory.Enabled),
		"backend: " + cfg.Memory.Retrieval.Backend,
		"write: " + cfg.Memory.Write.Mode,
		"",
		tuiPanelTitleStyle.Render("History"),
		fmt.Sprintf("enabled: %t", cfg.History.Enabled),
		"kind: " + cfg.History.Kind,
		fmt.Sprintf("messages: %d", len(m.messages)),
	}
	return trimLines(wrapLines(lines, width-4), height)
}

type tuiRoundStartedMsg struct {
	resp *claw.Response
	err  error
}

type tuiRoundEventMsg struct {
	resp *claw.Response
	ev   claw.Event
	err  error
}

func tuiStartRoundCmd(app *claw.Claw, text string) tea.Cmd {
	return func() tea.Msg {
		if app == nil {
			return tuiRoundStartedMsg{err: fmt.Errorf("claw app is nil")}
		}
		resp, err := app.RoundTrip(claw.Request{Text: text})
		return tuiRoundStartedMsg{resp: resp, err: err}
	}
}

func tuiReadRoundCmd(resp *claw.Response) tea.Cmd {
	return func() tea.Msg {
		ev, err := resp.Next()
		return tuiRoundEventMsg{resp: resp, ev: ev, err: err}
	}
}

type tuiRecallMsg struct {
	result tuiRecallResult
	err    error
}

func tuiRecallCmd(app *claw.Claw, query string) tea.Cmd {
	return func() tea.Msg {
		result, err := runTUIDebugRecall(app, query)
		return tuiRecallMsg{result: result, err: err}
	}
}

func runTUIDebugRecall(app *claw.Claw, query string) (tuiRecallResult, error) {
	if app == nil {
		return tuiRecallResult{}, fmt.Errorf("claw app is nil")
	}
	body, _ := json.Marshal(map[string]any{
		"text":  query,
		"top_k": 5,
	})
	req := httptest.NewRequest(http.MethodPost, "/debug/recall", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.ServeDebugHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return tuiRecallResult{}, fmt.Errorf("recall status %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var resp struct {
		Enabled bool           `json:"enabled"`
		Hits    []tuiRecallHit `json:"hits"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		return tuiRecallResult{}, err
	}
	return tuiRecallResult{Enabled: resp.Enabled, Query: query, Hits: resp.Hits}, nil
}

func loadTUIHistory(app *claw.Claw, contextID string) []tuiChatMessage {
	if app == nil {
		return nil
	}
	if strings.TrimSpace(contextID) == "" {
		contextID = "__default__"
	}
	req := httptest.NewRequest(http.MethodGet, "/debug/history?context_id="+contextID, nil)
	rec := httptest.NewRecorder()
	app.ServeDebugHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil
	}
	var resp struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		return nil
	}
	out := make([]tuiChatMessage, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		var text strings.Builder
		var label string
		for _, part := range msg.Parts {
			switch part.Type {
			case "text":
				text.WriteString(part.Text)
			case clawHistoryXMLPartType:
				label = firstNonEmpty(parseTUIXMLAttr(part.Text, "speaker", "name"), parseTUIXMLAttr(part.Text, "node", "id"), label)
			}
		}
		if strings.TrimSpace(text.String()) != "" {
			body := text.String()
			if isInternalHistoryMessage(msg.Role, label, body) {
				continue
			}
			out = append(out, tuiChatMessage{Role: msg.Role, Text: body, NodeID: label})
		}
	}
	return out
}

func isInternalHistoryMessage(role, name, text string) bool {
	if strings.TrimSpace(name) != "" || role != "assistant" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(text), "case_action:")
}

func parseTUIXMLAttr(text, nodeName, attrName string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<"+nodeName) || !strings.HasSuffix(text, "/>") {
		return ""
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "<"+nodeName), "/>"))
	prefix := attrName + "="
	idx := strings.Index(body, prefix)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(prefix):]
	if len(rest) == 0 || (rest[0] != '"' && rest[0] != '\'') {
		return ""
	}
	quote := rest[0]
	end := strings.IndexByte(rest[1:], quote)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(rest[1 : end+1]))
}

func latestAssistantText(messages any) string {
	raw, err := json.Marshal(messages)
	if err != nil {
		return ""
	}
	var decoded []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	for i := len(decoded) - 1; i >= 0; i-- {
		if decoded[i].Role != "assistant" {
			continue
		}
		var b strings.Builder
		for _, part := range decoded[i].Parts {
			if part.Type == "text" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return ""
}

func wrapLines(lines []string, width int) []string {
	var out []string
	for _, line := range lines {
		out = append(out, wrapLine(line, width)...)
	}
	return out
}

func wrapLine(line string, width int) []string {
	if width <= 0 || utf8.RuneCountInString(line) <= width {
		return []string{line}
	}
	var out []string
	runes := []rune(line)
	for len(runes) > width {
		out = append(out, string(runes[:width]))
		runes = runes[width:]
	}
	if len(runes) > 0 {
		out = append(out, string(runes))
	}
	return out
}

func trimLines(lines []string, height int) string {
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var (
	tuiTopStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Padding(0, 1)
	tuiPanelStyle      = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	tuiPanelTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	tuiHelpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Padding(0, 1)
	tuiErrorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)
