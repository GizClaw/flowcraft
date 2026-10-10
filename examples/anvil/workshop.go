package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/craft"
	"github.com/GizClaw/flowcraft/craft/hostmcp"
)

// workshop is anvil's one compile-time capability. It registers what
// craft.yaml references and nothing else: the registry is frozen by
// craft.New, so a capability decides what the definition is allowed to
// name, and a plugin can never add to it.
type workshop struct {
	mu   sync.Mutex
	emit func(ctx context.Context, pluginID, subject string, payload any) error
}

var _ hostmcp.EventService = (*workshop)(nil)

func newWorkshop() *workshop { return &workshop{} }

// Name implements craft.Capability.
func (*workshop) Name() string { return "workshop" }

// Register implements craft.Registrar. The three factory sets behind the
// deploy document: event.Bus/memory, tool.Assembly/memory and the
// example's own workshop.Notes/memory.
func (*workshop) Register(registry *resource.Registry) error {
	if err := event.Register(registry); err != nil {
		return err
	}
	if err := tool.Register(registry); err != nil {
		return err
	}
	return registry.Register(notesFactory{})
}

// RegisterServices implements craft.ServiceRegistrar, the named-service
// half of host_tools.services. craft.yaml selects this implementation
// with `services: {events: {impl: workshop}, secrets: {impl: workshop}}`.
func (w *workshop) RegisterServices(services *hostmcp.ServiceRegistry) error {
	if err := services.Register("events", "workshop", w); err != nil {
		return err
	}
	return services.Register("secrets", "workshop", newSecretStore())
}

// Emit implements hostmcp.EventService, the behavior behind the
// emit_event primitive.
func (w *workshop) Emit(ctx context.Context, pluginID, subject string, payload any) error {
	w.mu.Lock()
	emit := w.emit
	w.mu.Unlock()
	if emit == nil {
		return errors.New("workshop: no emitter is wired")
	}
	return emit(ctx, pluginID, subject, payload)
}

// setEmitter wires the primitive to the craft plane; the application
// calls it once the Craft exists, which is necessarily after New.
func (w *workshop) setEmitter(emit func(context.Context, string, string, any) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit = emit
}

// secretStore is the example's Secrets implementation: an in-memory
// store behind secret_get / secret_set / secret_delete, namespaced per
// plugin by the identity the host authenticates. It is the behavior the
// primitive family is bound to; the hello plugin has no secrets:auth
// grant, so host_about reports those primitives as denied for it.
type secretStore struct {
	mu     sync.Mutex
	values map[string]map[string]string
}

var _ hostmcp.SecretService = (*secretStore)(nil)

func newSecretStore() *secretStore {
	return &secretStore{values: map[string]map[string]string{}}
}

func (s *secretStore) Get(_ context.Context, pluginID, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[pluginID][name]
	if !ok {
		return "", fmt.Errorf("workshop: secret %q is not set for %s", name, pluginID)
	}
	return value, nil
}

func (s *secretStore) Set(_ context.Context, pluginID, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values[pluginID] == nil {
		s.values[pluginID] = map[string]string{}
	}
	s.values[pluginID][name] = value
	return nil
}

func (s *secretStore) Delete(_ context.Context, pluginID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values[pluginID], name)
	return nil
}

const (
	notesKind = "workshop.Notes"
	notesImpl = "memory"

	// runtimeDep is the dependency name of the always-injected
	// craft.runtime external.
	runtimeDep = "runtime"
)

// notesSettings is the strict settings subtree of workshop.Notes. The
// key avoids "file" and "embed": those two are the loader's
// whole-subtree reference forms, not settings.
type notesSettings struct {
	Path string `json:"path"`
}

// notesFactory builds one runtime's notes tool source. The craft.runtime
// external is injected into every runtime, so the value a factory
// returns is bound to the runtime key it was built for — which is how
// two runtimes end up with two independent notes files.
type notesFactory struct{}

func (notesFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: notesKind,
		Impl: notesImpl,
		Deps: []resource.DepSpec{{
			Name:     runtimeDep,
			Type:     craft.RuntimeContextContract,
			Required: true,
		}},
	}
}

func (notesFactory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[notesSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("workshop: decode notes settings: %w", err)
	}
	file := strings.TrimSpace(settings.Path)
	if file == "" {
		return nil, errors.New("workshop: notes.path is required")
	}
	value, ok := in.Dep(runtimeDep)
	if !ok {
		return nil, errors.New("workshop: the craft.runtime external is missing")
	}
	runtimeContext, ok := value.(craft.RuntimeContext)
	if !ok {
		return nil, fmt.Errorf("workshop: craft.runtime is %T", value)
	}
	return &notesSource{key: runtimeContext.Key, file: file}, nil
}

// notesSource is the resource value: a tool.Source whose two tools act
// on one runtime's notes file. Both tools carry the runtime key in their
// description, so the per-runtime catalog is visible to a model too.
type notesSource struct {
	key  craft.RuntimeKey
	file string
}

func (s *notesSource) Tools() []tool.Tool {
	return []tool.Tool{s.addTool(), s.listTool()}
}

// LazyTools implements tool.Source: nothing here needs to be loaded on
// first use, so the set is eager.
func (*notesSource) LazyTools() []tool.LazyTool { return nil }

func (s *notesSource) addTool() tool.Tool {
	return tool.TextTool(message.ToolDefinition{
		Name: "notes_add",
		Description: fmt.Sprintf(
			"Append one line to the %s runtime's notes file.", s.key),
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {"type": "string", "description": "one line of text"}
			},
			"required": ["text"],
			"additionalProperties": false
		}`),
	}, func(_ context.Context, arguments string) (string, error) {
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("notes_add: decode arguments: %w", err)
		}
		text := strings.TrimSpace(args.Text)
		if text == "" {
			return "", errors.New("notes_add: text is required")
		}
		if strings.ContainsAny(text, "\r\n") {
			return "", errors.New("notes_add: text must be a single line")
		}
		file, err := os.OpenFile(s.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return "", fmt.Errorf("notes_add: %w", err)
		}
		_, writeErr := fmt.Fprintln(file, text)
		closeErr := file.Close()
		if writeErr != nil {
			return "", fmt.Errorf("notes_add: %w", writeErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("notes_add: %w", closeErr)
		}
		notes, err := s.read()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("added note %d to %s", len(notes), s.file), nil
	})
}

func (s *notesSource) listTool() tool.Tool {
	return tool.TextTool(message.ToolDefinition{
		Name: "notes_list",
		Description: fmt.Sprintf(
			"List the %s runtime's notes file.", s.key),
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"additionalProperties": false
		}`),
	}, func(_ context.Context, _ string) (string, error) {
		notes, err := s.read()
		if err != nil {
			return "", err
		}
		switch len(notes) {
		case 0:
			return fmt.Sprintf("no notes yet in %s", s.file), nil
		case 1:
			return fmt.Sprintf("1 note in %s: %s", s.file, notes[0]), nil
		default:
			return fmt.Sprintf("%d notes in %s: %s",
				len(notes), s.file, strings.Join(notes, "; ")), nil
		}
	})
}

// read returns the file's non-empty lines, oldest first.
func (s *notesSource) read() ([]string, error) {
	raw, err := os.ReadFile(s.file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("notes: read %s: %w", s.file, err)
	}
	var notes []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			notes = append(notes, line)
		}
	}
	return notes, nil
}
