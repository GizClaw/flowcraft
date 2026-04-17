package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"

	otellog "go.opentelemetry.io/otel/log"
)

func requireGitWorkspace(ws workspace.Workspace) (workspace.GitWorkspace, error) {
	gw, ok := ws.(workspace.GitWorkspace)
	if !ok {
		return nil, errdefs.Validationf("git operations require a workspace with git support (e.g. LocalWorkspace)")
	}
	return gw, nil
}

// SkillStore indexes skills from a workspace directory and provides
// search and retrieval. Thread-safe via sync.RWMutex.
type SkillStore struct {
	ws        workspace.Workspace
	prefix    string
	builtinFS fs.FS // embedded built-in skills (optional)
	mu        sync.RWMutex
	skills    map[string]*SkillMeta // name -> meta
	readme    map[string]string     // name -> full SKILL.md content
	builtins  map[string]bool       // names of built-in skills

	tokenizer textsearch.Tokenizer
	corpus    *textsearch.CorpusStats
	docTokens map[string][]string // name -> tokenized search text
	skillsCfg config.SkillsConfig
	lockfile  *Lockfile
}

// NewSkillStore creates a store that scans for skills under the prefix directory.
// Install, Update, and UpdateAll need a GitWorkspace implementation (e.g. LocalWorkspace);
// indexing and search work with any Workspace.
func NewSkillStore(ws workspace.Workspace, prefix string) *SkillStore {
	if prefix == "" {
		prefix = "skills"
	}
	return &SkillStore{
		ws:        ws,
		prefix:    prefix,
		skills:    make(map[string]*SkillMeta),
		readme:    make(map[string]string),
		tokenizer: &textsearch.CJKTokenizer{},
		corpus:    textsearch.NewCorpusStats(),
		docTokens: make(map[string][]string),
		lockfile:  NewLockfile(ws, prefix),
	}
}

// SetBuiltinFS sets the embedded filesystem for built-in skills.
// Built-in skills are copied into the workspace on BuildIndex if they
// don't already exist. Built-in skills cannot be uninstalled.
func (s *SkillStore) SetBuiltinFS(fsys fs.FS) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.builtinFS = fsys
}

// IsBuiltin reports whether the named skill is a built-in skill.
func (s *SkillStore) IsBuiltin(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.builtins[name]
}

// SetGlobalConfig attaches global per-skill configuration (env, api_key, enabled).
func (s *SkillStore) SetGlobalConfig(cfg config.SkillsConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skillsCfg = cfg
}

// IsEnabled reports whether the named skill is enabled.
// A skill is enabled unless explicitly disabled via global config.
func (s *SkillStore) IsEnabled(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry, ok := s.skillsCfg.Entries[name]; ok && entry.Enabled != nil {
		return *entry.Enabled
	}
	return true
}

// ResolveEnv builds the environment variables for a skill execution.
// It merges the global config entry's Env map and maps APIKey to the
// skill's PrimaryEnv declaration.
func (s *SkillStore) ResolveEnv(name string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, hasEntry := s.skillsCfg.Entries[name]
	meta := s.skills[name]
	if !hasEntry && meta == nil {
		return nil
	}

	env := make(map[string]string)
	if hasEntry {
		for k, v := range entry.Env {
			env[k] = v
		}
		if entry.APIKey != "" && meta != nil && meta.PrimaryEnv != "" {
			env[meta.PrimaryEnv] = entry.APIKey
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// BuildIndex syncs built-in skills into the workspace (if not already
// present), then scans the skills directory and indexes all SKILL.md files.
func (s *SkillStore) BuildIndex(ctx context.Context) error {
	_ = s.lockfile.Load(ctx)
	builtins := s.syncBuiltins(ctx)

	entries, err := s.ws.List(ctx, s.prefix)
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("skill: list %s: %w", s.prefix, err)
	}

	skills := make(map[string]*SkillMeta)
	readme := make(map[string]string)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := entry.Name()
		metaPath := filepath.Join(s.prefix, skillDir, "SKILL.md")

		exists, err := s.ws.Exists(ctx, metaPath)
		if err != nil || !exists {
			continue
		}

		data, err := s.ws.Read(ctx, metaPath)
		if err != nil {
			continue
		}

		content := string(data)
		meta, err := ParseSkillMeta(content)
		if err != nil {
			telemetry.Warn(ctx, "skill: skipping invalid SKILL.md",
				otellog.String("dir", skillDir),
				otellog.String("error", err.Error()))
			continue
		}
		meta.Dir = filepath.Join(s.prefix, skillDir)
		meta.ReadmePath = metaPath

		if meta.Entry == "" {
			if entry := s.detectEntry(ctx, meta.Dir); entry != "" {
				meta.Entry = entry
			}
			// entry 仍为空 → 文档型 skill，正常索引
		}

		if builtins[meta.Name] {
			meta.Builtin = true
		}

		gating := evaluateGating(meta)
		meta.Gating = gating

		if !gating.Available && gating.Reason != "" {
			telemetry.Info(ctx, "skill: skipping due to gating",
				otellog.String("skill", meta.Name),
				otellog.String("reason", gating.Reason))
			continue
		}

		skills[meta.Name] = meta
		readme[meta.Name] = content
	}

	corpus := textsearch.NewCorpusStats()
	docTokens := make(map[string][]string, len(skills))
	for name, meta := range skills {
		tokens := s.tokenizer.Tokenize(skillSearchText(meta))
		corpus.AddDocument(tokens)
		docTokens[name] = tokens
	}

	s.mu.Lock()
	s.skills = skills
	s.readme = readme
	s.builtins = builtins
	s.corpus = corpus
	s.docTokens = docTokens
	s.mu.Unlock()
	return nil
}

// Get returns the metadata for a skill by name.
func (s *SkillStore) Get(name string) (*SkillMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.skills[name]
	if !ok {
		return nil, false
	}
	return copySkillMeta(meta), true
}

// GetReadme returns the full SKILL.md content for a skill.
func (s *SkillStore) GetReadme(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	content, ok := s.readme[name]
	return content, ok
}

// Search finds skills matching a keyword query, ranked by BM25 score.
// If whitelist is non-empty, only skills in the whitelist are considered.
func (s *SkillStore) Search(query string, whitelist []string) []*SkillMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	wl := make(map[string]bool, len(whitelist))
	for _, name := range whitelist {
		wl[name] = true
	}

	keywords := textsearch.ExtractKeywords(query, s.tokenizer)
	if len(keywords) == 0 {
		return nil
	}

	type scored struct {
		meta  *SkillMeta
		score float64
	}
	var results []scored
	for name, meta := range s.skills {
		if len(wl) > 0 && !wl[name] {
			continue
		}
		tokens := s.docTokens[name]
		score := textsearch.BM25(tokens, keywords, s.corpus)
		if score > 0 {
			results = append(results, scored{meta: copySkillMeta(meta), score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	out := make([]*SkillMeta, len(results))
	for i, r := range results {
		out[i] = r.meta
	}
	return out
}

// List returns all indexed skills, optionally filtered by whitelist.
func (s *SkillStore) List(whitelist []string) []*SkillMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	wl := make(map[string]bool, len(whitelist))
	for _, name := range whitelist {
		wl[name] = true
	}

	var results []*SkillMeta
	for name, meta := range s.skills {
		if len(wl) > 0 && !wl[name] {
			continue
		}
		results = append(results, copySkillMeta(meta))
	}
	return results
}

// detectEntry probes the skill directory for common entry point files.
var defaultEntryFiles = []string{
	"main.py", "index.js", "index.ts", "main.go", "run.sh", "main.sh",
}

func (s *SkillStore) detectEntry(ctx context.Context, dir string) string {
	for _, name := range defaultEntryFiles {
		path := filepath.Join(dir, name)
		exists, err := s.ws.Exists(ctx, path)
		if err == nil && exists {
			return name
		}
	}
	return ""
}

// findByDir searches for a skill by its directory path (for when meta.Name != targetDir).
func (s *SkillStore) findByDir(dir string) (*SkillMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, meta := range s.skills {
		if meta.Dir == dir {
			return copySkillMeta(meta), true
		}
	}
	return nil, false
}

func skillSearchText(meta *SkillMeta) string {
	return meta.Name + " " + meta.Description + " " + strings.Join(meta.Tags, " ")
}

// Install clones a skill from a git URL into the workspace.
func (s *SkillStore) Install(ctx context.Context, url, name string) error {
	gw, err := requireGitWorkspace(s.ws)
	if err != nil {
		return err
	}

	// Determine target directory name
	targetDir := name
	if targetDir == "" {
		// Extract repo name from URL
		parts := strings.Split(strings.TrimSuffix(url, ".git"), "/")
		targetDir = parts[len(parts)-1]
	}

	dest := filepath.Join(s.prefix, targetDir)

	// Check if already exists
	exists, err := s.ws.Exists(ctx, dest)
	if err != nil {
		return errdefs.Internal(fmt.Errorf("check existence: %w", err))
	}
	if exists {
		return errdefs.Conflictf("skill already installed: %s", targetDir)
	}

	// Clone the repository
	if err := gw.GitClone(ctx, url, dest); err != nil {
		return errdefs.Internal(fmt.Errorf("git clone %s: %w", url, err))
	}

	// Rebuild index to include the new skill
	if err := s.BuildIndex(ctx); err != nil {
		return errdefs.Internal(fmt.Errorf("rebuild index: %w", err))
	}

	meta, ok := s.Get(targetDir)
	if !ok {
		meta, ok = s.findByDir(filepath.Join(s.prefix, targetDir))
	}
	if !ok {
		return fmt.Errorf("skill installed but not indexed (possible causes: unsupported OS %s, or missing/invalid SKILL.md)", runtime.GOOS)
	}
	if meta.Gating != nil && !meta.Gating.Available {
		telemetry.Warn(ctx, "skill: installed but unavailable",
			otellog.String("skill", meta.Name),
			otellog.String("missing", formatGatingMessage(meta)))
	}

	commit, _ := gw.GitHead(ctx, dest)
	s.lockfile.Set(meta.Name, &SkillSource{GitURL: url, Commit: commit})
	_ = s.lockfile.Save(ctx)

	return nil
}

// Uninstall removes a skill from the workspace.
// Built-in skills cannot be uninstalled.
func (s *SkillStore) Uninstall(ctx context.Context, name string) error {
	meta, ok := s.Get(name)
	if !ok {
		return errdefs.NotFoundf("skill %q not found", name)
	}
	if s.IsBuiltin(name) {
		return errdefs.Validationf("built-in skill %q cannot be uninstalled", name)
	}

	if err := s.ws.RemoveAll(ctx, meta.Dir); err != nil {
		return errdefs.Internal(fmt.Errorf("uninstall skill: %w", err))
	}

	s.lockfile.Remove(name)
	_ = s.lockfile.Save(ctx)

	if err := s.BuildIndex(ctx); err != nil {
		return errdefs.Internal(fmt.Errorf("rebuild index: %w", err))
	}

	return nil
}

// Update pulls the latest changes for a Git-installed skill.
func (s *SkillStore) Update(ctx context.Context, name string) error {
	gw, err := requireGitWorkspace(s.ws)
	if err != nil {
		return err
	}

	meta, ok := s.Get(name)
	if !ok {
		return errdefs.NotFoundf("skill %q not found", name)
	}
	if s.IsBuiltin(name) {
		return errdefs.Validationf("built-in skill %q cannot be updated via git", name)
	}

	source := s.lockfile.Get(name)
	if source == nil {
		return errdefs.Validationf("skill %q has no git source in lockfile", name)
	}

	if err := gw.GitPull(ctx, meta.Dir); err != nil {
		return errdefs.Internal(fmt.Errorf("git pull %s: %w", name, err))
	}

	commit, _ := gw.GitHead(ctx, meta.Dir)
	s.lockfile.UpdateCommit(name, commit)
	_ = s.lockfile.Save(ctx)

	return s.BuildIndex(ctx)
}

// UpdateAll pulls latest changes for all Git-installed skills.
func (s *SkillStore) UpdateAll(ctx context.Context) ([]string, error) {
	var updated []string
	s.mu.RLock()
	var names []string
	for name := range s.skills {
		if !s.builtins[name] && s.lockfile.Get(name) != nil {
			names = append(names, name)
		}
	}
	s.mu.RUnlock()

	var errs []error
	for _, name := range names {
		if err := s.Update(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else {
			updated = append(updated, name)
		}
	}
	if len(errs) > 0 {
		return updated, errors.Join(errs...)
	}
	return updated, nil
}

// GetSource returns the lockfile source info for a skill, or nil.
func (s *SkillStore) GetSource(name string) *SkillSource {
	return s.lockfile.Get(name)
}

// syncBuiltins copies embedded built-in skills into the workspace.
// Skills that already exist in the workspace are skipped.
// It also records built-in skill names so they can be marked in the index.
func (s *SkillStore) syncBuiltins(ctx context.Context) map[string]bool {
	if s.builtinFS == nil {
		return nil
	}

	entries, err := fs.ReadDir(s.builtinFS, ".")
	if err != nil {
		return nil
	}

	builtins := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		builtins[skillName] = true

		dest := filepath.Join(s.prefix, skillName)
		exists, _ := s.ws.Exists(ctx, dest)
		if exists {
			continue
		}

		skillFiles, err := fs.ReadDir(s.builtinFS, skillName)
		if err != nil {
			continue
		}
		for _, f := range skillFiles {
			if f.IsDir() {
				continue
			}
			data, err := fs.ReadFile(s.builtinFS, filepath.Join(skillName, f.Name()))
			if err != nil {
				continue
			}
			filePath := filepath.Join(dest, f.Name())
			if err := s.ws.Write(ctx, filePath, data); err != nil {
				telemetry.Warn(ctx, "skill: failed to sync builtin file",
					otellog.String("skill", skillName),
					otellog.String("file", f.Name()),
					otellog.String("error", err.Error()))
			}
		}
	}
	return builtins
}
