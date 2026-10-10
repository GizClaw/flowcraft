package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// Event names. The vocabulary is a platform ABI: frozen and
// append-only. See the package documentation for the transport
// contract each event's payload follows.
const (
	EventPreToolUse        = "PreToolUse"
	EventPostToolUse       = "PostToolUse"
	EventUserPromptSubmit  = "UserPromptSubmit"
	EventPermissionRequest = "PermissionRequest"
	EventTurnEnd           = "TurnEnd"
	EventSessionStart      = "SessionStart"
	EventSessionEnd        = "SessionEnd"
	EventSubagentStart     = "SubagentStart"
	EventSubagentStop      = "SubagentStop"
)

const (
	// defaultTimeout bounds a command hook whose config omits one.
	defaultTimeout = 30 * time.Second
	// maxOutput is how much combined output a hook run retains for its
	// log line. A hook that writes more than this is not blocked: the
	// excess is counted as truncated and dropped.
	maxOutput = 64 << 10
	// waitDelay bounds the wait for the output pipe to close once the
	// command (or its process group) is gone, so a hook that leaves a
	// background process holding the pipe cannot stall the run.
	waitDelay = time.Second
	// commandShell interprets every hook command.
	commandShell = "sh"
)

// eventNames is the canonical order [EventNames] reports.
var eventNames = []string{
	EventPreToolUse,
	EventPostToolUse,
	EventUserPromptSubmit,
	EventPermissionRequest,
	EventTurnEnd,
	EventSessionStart,
	EventSessionEnd,
	EventSubagentStart,
	EventSubagentStop,
}

// EventNames returns the event vocabulary in canonical order.
func EventNames() []string {
	return append([]string(nil), eventNames...)
}

// Hook is one command invocation declared in a hooks.json group.
type Hook struct {
	// Type selects the hook kind. Empty and "command" are command
	// hooks; any other value is skipped when the file loads, so a
	// config written for a later runner version still loads here.
	Type string `json:"type"`
	// Command is the shell command line, run under sh -c.
	Command string `json:"command,omitempty"`
	// Timeout in seconds. Zero or negative means [defaultTimeout].
	Timeout int `json:"timeout,omitempty"`
}

// Group matches one event occurrence and runs its hooks in order.
type Group struct {
	// Matcher is a regex tested against the occurrence's match value,
	// the first non-empty of the payload's tool / source / reason /
	// subagent fields. Empty and "*" match every occurrence.
	Matcher string `json:"matcher,omitempty"`
	// Hooks are the commands this group runs.
	Hooks []Hook `json:"hooks"`
}

// ExtraSource is one additional hooks.json file, normally contributed
// by a plugin. Dir anchors the file's commands: it becomes their
// working directory. Trusted marks a source whose commands may see the
// full payload; the zero value strips the content-bearing fields
// before the command runs.
type ExtraSource struct {
	Path    string
	Dir     string
	Trusted bool
}

// configFile is the on-disk hooks.json shape.
type configFile struct {
	Hooks map[string][]Group `json:"hooks"`
}

// Manager is a loaded set of hook groups, one runner per deployment.
// It is safe for concurrent use.
type Manager struct {
	path   string
	groups map[string][]groupEntry
}

// groupEntry is one loaded group: a compiled matcher plus the source
// facts its hooks inherit.
type groupEntry struct {
	re      *regexp.Regexp
	hooks   []Hook
	dir     string
	trusted bool
}

// Load parses the hooks.json file at path. A missing file is not an
// error: it yields a manager with no groups.
func Load(ctx context.Context, path string) (*Manager, error) {
	return LoadWithSources(ctx, path, nil)
}

// LoadWithSources parses the hooks.json file at path plus every extra
// source. A missing primary file is fine; a primary file that does not
// parse is an error, because a user's own misconfiguration should be
// loud. An extra source that cannot be read or parsed is skipped with a
// warning instead: a broken plugin must not take the runner down with
// it.
func LoadWithSources(
	ctx context.Context, path string, extra []ExtraSource,
) (*Manager, error) {
	m := &Manager{path: path, groups: map[string][]groupEntry{}}
	if err := m.loadFile(path, "", true, true); err != nil {
		return nil, err
	}
	for _, source := range extra {
		if strings.TrimSpace(source.Path) == "" {
			continue
		}
		if err := m.loadFile(source.Path, source.Dir, source.Trusted, false); err != nil {
			telemetry.Warn(ctx, "hooks: skipping hook source",
				attribute.String("path", source.Path),
				attribute.String("error", err.Error()))
		}
	}
	return m, nil
}

// loadFile reads one hooks.json into the manager. optional sources may
// be absent, required ones report it.
func (m *Manager) loadFile(
	path, dir string, trusted, optional bool,
) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("hooks: read %s: %w", path, err)
	}
	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return errdefs.Validationf("hooks: parse %s: %v", path, err)
	}
	for _, event := range sortedKeys(cfg.Hooks) {
		if !knownEvent(event) {
			return errdefs.Validationf(
				"hooks: %s: unknown event %q; known events are %s",
				path, event, strings.Join(eventNames, ", "))
		}
		for i, group := range cfg.Hooks[event] {
			entry, err := newGroupEntry(group, dir, trusted)
			if err != nil {
				return errdefs.Validationf(
					"hooks: %s: %s[%d]: %v", path, event, i, err)
			}
			if entry != nil {
				m.groups[event] = append(m.groups[event], *entry)
			}
		}
	}
	return nil
}

// newGroupEntry compiles one group, dropping the hook kinds this runner
// does not run. A group left with no hooks is reported as nil.
func newGroupEntry(
	group Group, dir string, trusted bool,
) (*groupEntry, error) {
	var re *regexp.Regexp
	matcher := strings.TrimSpace(group.Matcher)
	if matcher != "" && matcher != "*" {
		compiled, err := regexp.Compile(matcher)
		if err != nil {
			return nil, fmt.Errorf("matcher: %w", err)
		}
		re = compiled
	}
	hooks := make([]Hook, 0, len(group.Hooks))
	for _, hook := range group.Hooks {
		if hook.Type != "" && hook.Type != "command" {
			continue
		}
		if strings.TrimSpace(hook.Command) == "" {
			return nil, errdefs.Validationf("hook command is required")
		}
		hooks = append(hooks, hook)
	}
	if len(hooks) == 0 {
		return nil, nil
	}
	return &groupEntry{re: re, hooks: hooks, dir: dir, trusted: trusted}, nil
}

// sortedKeys returns a map's string keys in sorted order, so loading
// reports the first error in file order instead of map order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// knownEvent reports whether event is part of the frozen vocabulary.
func knownEvent(event string) bool {
	for _, name := range eventNames {
		if name == event {
			return true
		}
	}
	return false
}

// Path returns the primary hooks.json path.
func (m *Manager) Path() string { return m.path }

// Empty reports whether the manager holds no hook groups at all.
func (m *Manager) Empty() bool { return len(m.groups) == 0 }

// Fire runs every group matching event and payload, in load order,
// running each group's hooks in declaration order. It never returns an
// error: hook failures are logged and skipped, so user hooks can never
// block the agent loop.
//
// payload carries the event's fields. Its values are read, never
// mutated, and the runner stamps "event" itself.
func (m *Manager) Fire(ctx context.Context, event string, payload map[string]any) {
	if m == nil || len(m.groups) == 0 {
		return
	}
	entries := m.groups[event]
	if len(entries) == 0 {
		return
	}
	value := matchValue(payload)
	for _, entry := range entries {
		if entry.re != nil && !entry.re.MatchString(value) {
			continue
		}
		for _, hook := range entry.hooks {
			m.run(ctx, event, entry, hook, payload)
		}
	}
}

// matchValue returns the field a matcher regex is tested against:
// tool events match "tool", session events "source" or "reason",
// subagent events "subagent".
func matchValue(payload map[string]any) string {
	for _, key := range []string{"tool", "source", "reason", "subagent"} {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
