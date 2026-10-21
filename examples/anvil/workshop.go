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
	pluginserver "github.com/GizClaw/flowcraft/craft/plugin/server"
)

// workshop is anvil's one compile-time capability. It registers what
// craft.yaml references and nothing else: the registry is frozen by
// craft.New, so a capability decides what the definition is allowed to
// name, and a plugin can never add to it.
//
// It is also the application's half of the plugin wire. Craft ships no
// primitives, so the two a plugin may call — events.emit and the secret
// ones — are declared here, next to the behavior behind them.
type workshop struct {
	mu      sync.Mutex
	emit    func(ctx context.Context, pluginID, subject string, payload any) error
	secrets *secretStore
}

var _ craft.HostPrimitives = (*workshop)(nil)

func newWorkshop() *workshop {
	return &workshop{secrets: newSecretStore()}
}

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
	return resource.Register(registry, notesFactory{})
}

// HostPrimitives implements craft.HostPrimitives. The grant names are the
// permissions a plugin manifest asks for: a plugin that declares
// events:emit can publish, and one that does not is told by host.about
// that the primitive is there and needs a grant it does not hold.
func (w *workshop) HostPrimitives() []pluginserver.Primitive {
	return []pluginserver.Primitive{{
		Resource: "events", Op: "emit", Grant: "events:emit",
		Description: "Publish one event on the craft plane for the calling plugin",
		Params:      json.RawMessage(paramsEvent),
		Handler:     w.emitEvent,
	}, {
		Resource: "secret", Op: "get", Grant: "secrets:auth",
		Description: "Read a secret from the calling plugin's namespace",
		Params:      json.RawMessage(paramsSecretName),
		Handler:     w.secrets.get,
	}, {
		Resource: "secret", Op: "set", Grant: "secrets:auth",
		Description: "Store a secret in the calling plugin's namespace",
		Params:      json.RawMessage(paramsSecretSet),
		Handler:     w.secrets.set,
	}, {
		Resource: "secret", Op: "delete", Grant: "secrets:auth",
		Description: "Delete a secret from the calling plugin's namespace",
		Params:      json.RawMessage(paramsSecretName),
		Handler:     w.secrets.del,
	}}
}

// The params schemas host.about publishes for the workshop's primitives.
// They are documentation, not enforcement: each handler still refuses a
// missing parameter by name.
const (
	paramsEvent = `{"type":"object","properties":{` +
		`"subject":{"type":"string"},"payload":{}},"required":["subject"]}`
	paramsSecretName = `{"type":"object","properties":{` +
		`"name":{"type":"string"}},"required":["name"]}`
	paramsSecretSet = `{"type":"object","properties":{` +
		`"name":{"type":"string"},"value":{"type":"string"}},` +
		`"required":["name","value"]}`
)

// emitEvent is the behavior behind events.emit.
func (w *workshop) emitEvent(ctx context.Context, call pluginserver.Call) (any, error) {
	subject, err := call.String("subject")
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	emit := w.emit
	w.mu.Unlock()
	if emit == nil {
		return nil, errors.New("workshop: no emitter is wired")
	}
	if err := emit(ctx, call.Identity.PluginID, subject, call.Params["payload"]); err != nil {
		return nil, err
	}
	return map[string]any{}, nil
}

// setEmitter wires the primitive to the craft plane; the application
// calls it once the Craft exists, which is necessarily after New.
func (w *workshop) setEmitter(emit func(context.Context, string, string, any) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit = emit
}

// secretStore is an in-memory store behind secret.get / secret.set /
// secret.delete, namespaced per plugin by the identity the connection
// carries. The hello plugin has no secrets:auth grant, so host.about
// reports those primitives as denied for it.
type secretStore struct {
	mu     sync.Mutex
	values map[string]map[string]string
}

func newSecretStore() *secretStore {
	return &secretStore{values: map[string]map[string]string{}}
}

func (s *secretStore) get(_ context.Context, call pluginserver.Call) (any, error) {
	name, err := call.String("name")
	if err != nil {
		return nil, err
	}
	pluginID := call.Identity.PluginID
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[pluginID][name]
	if !ok {
		return nil, fmt.Errorf(
			"workshop: secret %q is not set for %s", name, pluginID)
	}
	return map[string]any{"value": value}, nil
}

func (s *secretStore) set(_ context.Context, call pluginserver.Call) (any, error) {
	name, err := call.String("name")
	if err != nil {
		return nil, err
	}
	value, err := call.Text("value")
	if err != nil {
		return nil, err
	}
	pluginID := call.Identity.PluginID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values[pluginID] == nil {
		s.values[pluginID] = map[string]string{}
	}
	s.values[pluginID][name] = value
	return map[string]any{}, nil
}

func (s *secretStore) del(_ context.Context, call pluginserver.Call) (any, error) {
	name, err := call.String("name")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values[call.Identity.PluginID], name)
	return map[string]any{}, nil
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

// depRuntime is the injected craft.runtime external: the typed handle names
// the contract once, and the plan checks it against the injected value.
var depRuntime = resource.Require[craft.RuntimeContext](runtimeDep)

func (notesFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: notesKind,
		Impl: notesImpl,
		Deps: []resource.Requirement{depRuntime.Spec()},
	}
}

func (notesFactory) New(ctx context.Context, in resource.Input) (*notesSource, error) {
	settings, err := resource.DecodeTyped[notesSettings](ctx, in.Settings)
	if err != nil {
		return nil, fmt.Errorf("workshop: decode notes settings: %w", err)
	}
	file := strings.TrimSpace(settings.Path)
	if file == "" {
		return nil, errors.New("workshop: notes.path is required")
	}
	runtimeContext, err := depRuntime.Get(in)
	if err != nil {
		return nil, fmt.Errorf("workshop: craft.runtime: %w", err)
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
