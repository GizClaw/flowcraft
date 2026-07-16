package claw

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestHistoryUsesWorkspaceFileStoreByDefault(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	writeHistoryTestConfig(t, ws, true, "custom-history")

	app, err := New(ws)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	if err := app.history.appendTurn(ctx, "conversation", model.NewTextMessage(model.RoleUser, "hello"), []model.Message{
		model.NewTextMessage(model.RoleAssistant, "hi"),
	}); err != nil {
		t.Fatalf("appendTurn: %v", err)
	}

	exists, err := ws.Exists(ctx, "custom-history/conversation/messages.jsonl")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("default history file does not exist")
	}
}

func TestHistoryUsesInjectedStoreWithoutWorkspacePrefix(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	writeHistoryTestConfig(t, ws, true, "must-not-be-used")
	store := newRecordingHistoryStore()

	app, err := New(ws, WithHistoryStore(store))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	want := []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
		model.NewTextMessage(model.RoleAssistant, "hi"),
	}
	if err := app.history.appendTurn(ctx, "conversation", want[0], want[1:]); err != nil {
		t.Fatalf("appendTurn: %v", err)
	}
	got, err := app.history.load(ctx, "conversation")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %#v, want %#v", got, want)
	}

	exists, err := ws.Exists(ctx, "must-not-be-used/conversation/messages.jsonl")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("injected Store unexpectedly wrote to workspace history root")
	}
}

func TestDisabledHistoryDoesNotAccessInjectedStore(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	writeHistoryTestConfig(t, ws, false, "history")
	store := newRecordingHistoryStore()

	app, err := New(ws, WithHistoryStore(store))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	if app.history != nil {
		t.Fatal("history runtime is enabled")
	}
	if got := store.callCount(); got != 0 {
		t.Fatalf("injected Store calls = %d, want 0", got)
	}
}

func TestNilHistoryStoreUsesWorkspaceDefault(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	writeHistoryTestConfig(t, ws, true, "history")

	app, err := New(ws, WithHistoryStore(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	ctx := context.Background()
	if err := app.history.appendTurn(ctx, "conversation", model.NewTextMessage(model.RoleUser, "hello"), nil); err != nil {
		t.Fatalf("appendTurn: %v", err)
	}
	exists, err := ws.Exists(ctx, "history/conversation/messages.jsonl")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("default history file does not exist")
	}
}

func TestInjectedHistoryStoreErrorsPropagate(t *testing.T) {
	errLoad := errors.New("load failed")
	errSave := errors.New("save failed")

	for _, tc := range []struct {
		name     string
		store    *recordingHistoryStore
		exercise func(*historyRuntime) error
		want     error
	}{
		{
			name:  "load",
			store: &recordingHistoryStore{loadErr: errLoad},
			exercise: func(history *historyRuntime) error {
				_, err := history.load(context.Background(), "conversation")
				return err
			},
			want: errLoad,
		},
		{
			name:  "append",
			store: &recordingHistoryStore{saveErr: errSave},
			exercise: func(history *historyRuntime) error {
				return history.appendTurn(context.Background(), "conversation", model.NewTextMessage(model.RoleUser, "hello"), nil)
			},
			want: errSave,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := workspace.NewMemWorkspace()
			writeHistoryTestConfig(t, ws, true, "history")
			app, err := New(ws, WithHistoryStore(tc.store))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer app.Close()

			if err := tc.exercise(app.history); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func writeHistoryTestConfig(t *testing.T, ws workspace.Workspace, enabled bool, root string) {
	t.Helper()
	cfg := defaultConfig()
	cfg.Memory.Enabled = false
	cfg.History.Enabled = enabled
	cfg.History.Kind = "buffer"
	cfg.Workspace.HistoryRoot = root
	writeTestConfig(t, ws, cfg)
}

type recordingHistoryStore struct {
	mu       sync.Mutex
	messages map[string][]model.Message
	loadErr  error
	saveErr  error
	calls    int
}

func newRecordingHistoryStore() *recordingHistoryStore {
	return &recordingHistoryStore{messages: make(map[string][]model.Message)}
}

func (s *recordingHistoryStore) GetMessages(_ context.Context, conversationID string) ([]model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return append([]model.Message(nil), s.messages[conversationID]...), nil
}

func (s *recordingHistoryStore) SaveMessages(_ context.Context, conversationID string, messages []model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.messages == nil {
		s.messages = make(map[string][]model.Message)
	}
	s.messages[conversationID] = append([]model.Message(nil), messages...)
	return nil
}

func (s *recordingHistoryStore) DeleteMessages(_ context.Context, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	delete(s.messages, conversationID)
	return nil
}

func (s *recordingHistoryStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
