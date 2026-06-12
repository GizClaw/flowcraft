package cli

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTUISelectorSelectsWithKeyboard(t *testing.T) {
	model := newTUISelectorModel("Select", []tuiSelectItem{
		{Title: "chat", Value: "chat"},
		{Title: "journey", Value: "journey"},
	})
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(tuiSelectorModel)
	if model.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", model.cursor)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(tuiSelectorModel)
	if model.selected == nil || model.selected.Value != "journey" {
		t.Fatalf("selected = %+v, want journey", model.selected)
	}
}

func TestTUISelectorViewRendersItems(t *testing.T) {
	model := newTUISelectorModel("Select raid config", []tuiSelectItem{{Title: "chat", Desc: "config"}})
	view := model.View()
	if !strings.Contains(view, "Select raid config") || !strings.Contains(view, "chat") {
		t.Fatalf("view = %q, want title and item", view)
	}
}
