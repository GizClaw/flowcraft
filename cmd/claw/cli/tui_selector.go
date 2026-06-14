package cli

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type tuiSelectItem struct {
	Title string
	Desc  string
	Value string
}

type tuiSelectorModel struct {
	title    string
	items    []tuiSelectItem
	cursor   int
	selected *tuiSelectItem
	canceled bool
	width    int
	height   int
}

func newTUISelectorModel(title string, items []tuiSelectItem) tuiSelectorModel {
	return tuiSelectorModel{
		title: strings.TrimSpace(title),
		items: append([]tuiSelectItem(nil), items...),
	}
}

func (m tuiSelectorModel) Init() tea.Cmd {
	return nil
}

func (m tuiSelectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
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

func (m tuiSelectorModel) View() string {
	var b strings.Builder
	title := m.title
	if title == "" {
		title = "Select"
	}
	b.WriteString(selectorTitleStyle.Render(title))
	b.WriteString("\n\n")
	if len(m.items) == 0 {
		b.WriteString("No items found.\n")
		return b.String()
	}
	for i, item := range m.items {
		cursor := "  "
		lineStyle := selectorItemStyle
		if i == m.cursor {
			cursor = "> "
			lineStyle = selectorSelectedStyle
		}
		line := cursor + item.Title
		if item.Desc != "" {
			line += "  " + selectorDescStyle.Render(item.Desc)
		}
		b.WriteString(lineStyle.Render(line))
		b.WriteByte('\n')
	}
	b.WriteString("\n")
	b.WriteString(selectorHelpStyle.Render("up/down select  enter open  q quit"))
	return b.String()
}

func runTUISelector(title string, items []tuiSelectItem) (tuiSelectItem, bool, error) {
	if len(items) == 0 {
		return tuiSelectItem{}, false, fmt.Errorf("no items available")
	}
	program := tea.NewProgram(newTUISelectorModel(title, items))
	model, err := program.Run()
	if err != nil {
		return tuiSelectItem{}, false, err
	}
	selector, ok := model.(tuiSelectorModel)
	if !ok || selector.canceled || selector.selected == nil {
		return tuiSelectItem{}, false, nil
	}
	return *selector.selected, true, nil
}

var (
	selectorTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	selectorItemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	selectorSelectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)
	selectorDescStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selectorHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)
