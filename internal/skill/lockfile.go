package skill

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// SkillSource tracks installation metadata for a Git-installed skill.
type SkillSource struct {
	GitURL      string `json:"git_url"`
	Commit      string `json:"commit,omitempty"`
	InstalledAt string `json:"installed_at"`
}

// Lockfile manages the skills lockfile for tracking installed Git skills.
type Lockfile struct {
	mu      sync.RWMutex
	ws      workspace.Workspace
	path    string
	entries map[string]*SkillSource
}

// NewLockfile creates a Lockfile backed by the given workspace.
// The lockfile is stored at <prefix>/.lockfile.json.
func NewLockfile(ws workspace.Workspace, prefix string) *Lockfile {
	return &Lockfile{
		ws:      ws,
		path:    filepath.Join(prefix, ".lockfile.json"),
		entries: make(map[string]*SkillSource),
	}
}

// Load reads the lockfile from workspace. Missing file is not an error.
func (l *Lockfile) Load(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := l.ws.Read(ctx, l.path)
	if err != nil {
		l.entries = make(map[string]*SkillSource)
		return nil
	}
	var entries map[string]*SkillSource
	if err := json.Unmarshal(data, &entries); err != nil {
		l.entries = make(map[string]*SkillSource)
		return nil
	}
	l.entries = entries
	return nil
}

// Save persists the lockfile to workspace.
func (l *Lockfile) Save(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return err
	}
	return l.ws.Write(ctx, l.path, data)
}

// Set records or updates a skill source entry.
func (l *Lockfile) Set(name string, source *SkillSource) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if source.InstalledAt == "" {
		source.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	}
	l.entries[name] = source
}

// Remove deletes a skill source entry.
func (l *Lockfile) Remove(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, name)
}

// Get returns the source for a named skill, or nil.
func (l *Lockfile) Get(name string) *SkillSource {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.entries[name]
}

// UpdateCommit updates the commit hash for a tracked skill.
func (l *Lockfile) UpdateCommit(name, commit string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[name]; ok {
		e.Commit = commit
	}
}
