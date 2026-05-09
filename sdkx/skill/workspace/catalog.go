// Package workspace provides a workspace-backed Agent Skills catalog.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	sdkskill "github.com/GizClaw/flowcraft/sdk/skill"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"

	"gopkg.in/yaml.v2"
)

const defaultRoot = "skills"

// Option configures a Catalog.
type Option func(*Catalog)

// WithRoot sets the workspace directory that contains skill folders.
func WithRoot(root string) Option {
	return func(c *Catalog) {
		if root != "" {
			c.root = root
		}
	}
}

// WithTokenizer overrides the BM25 tokenizer used by Search.
func WithTokenizer(t textsearch.Tokenizer) Option {
	return func(c *Catalog) {
		if t != nil {
			c.tokenizer = t
		}
	}
}

// Catalog indexes Agent Skills stored under <root>/<skill>/SKILL.md.
type Catalog struct {
	ws        sdkworkspace.Workspace
	root      string
	tokenizer textsearch.Tokenizer

	mu        sync.RWMutex
	indexed   bool
	skills    map[string]*sdkskill.Skill
	corpus    *textsearch.CorpusStats
	docTokens map[string][]string
}

// New creates a workspace-backed skills catalog. It indexes lazily on
// first use; call Refresh to force a scan at a controlled time.
func New(ws sdkworkspace.Workspace, opts ...Option) *Catalog {
	c := &Catalog{
		ws:        ws,
		root:      defaultRoot,
		tokenizer: &textsearch.CJKTokenizer{},
		skills:    map[string]*sdkskill.Skill{},
		corpus:    textsearch.NewCorpusStats(),
		docTokens: map[string][]string{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Refresh rebuilds the in-memory index from workspace contents.
func (c *Catalog) Refresh(ctx context.Context) error {
	entries, err := c.ws.List(ctx, c.root)
	if err != nil {
		if errors.Is(err, sdkworkspace.ErrNotFound) {
			c.replaceIndex(map[string]*sdkskill.Skill{})
			return nil
		}
		return fmt.Errorf("skill workspace: list %s: %w", c.root, err)
	}

	skills := make(map[string]*sdkskill.Skill)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sk, err := c.readSkill(ctx, entry)
		if err != nil {
			continue
		}
		skills[sk.Name] = sk
	}
	c.replaceIndex(skills)
	return nil
}

func (c *Catalog) replaceIndex(skills map[string]*sdkskill.Skill) {
	corpus := textsearch.NewCorpusStats()
	docTokens := make(map[string][]string, len(skills))
	for name, sk := range skills {
		tokens := c.tokenizer.Tokenize(searchText(sk))
		corpus.AddDocument(tokens)
		docTokens[name] = tokens
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.skills = skills
	c.corpus = corpus
	c.docTokens = docTokens
	c.indexed = true
}

func (c *Catalog) ensureIndex(ctx context.Context) error {
	c.mu.RLock()
	indexed := c.indexed
	c.mu.RUnlock()
	if indexed {
		return nil
	}
	return c.Refresh(ctx)
}

func (c *Catalog) readSkill(ctx context.Context, entry fs.DirEntry) (*sdkskill.Skill, error) {
	dir := filepath.Join(c.root, entry.Name())
	readmePath := filepath.Join(dir, "SKILL.md")
	data, err := c.ws.Read(ctx, readmePath)
	if err != nil {
		return nil, err
	}
	parsed, err := parseSkillMarkdown(string(data))
	if err != nil {
		return nil, err
	}
	parsed.Root = dir
	parsed.Path = readmePath
	parsed.Gating = evaluateGating(parsed.Requires)
	parsed.Available = parsed.Gating == nil || parsed.Gating.Available
	parsed.MissingDeps = missingDeps(parsed.Gating)
	return parsed, nil
}

// List returns all indexed skills, optionally filtered by whitelist.
func (c *Catalog) List(ctx context.Context, opts sdkskill.ListOptions) ([]sdkskill.Summary, error) {
	if err := c.ensureIndex(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	allow := allowSet(opts.Whitelist)
	out := make([]sdkskill.Summary, 0, len(c.skills))
	for name, sk := range c.skills {
		if len(allow) > 0 && !allow[name] {
			continue
		}
		out = append(out, copySummary(sk.Summary))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Search returns BM25-ranked skills for query.
func (c *Catalog) Search(ctx context.Context, query string, opts sdkskill.SearchOptions) ([]sdkskill.Summary, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if err := c.ensureIndex(ctx); err != nil {
		return nil, err
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	keywords := textsearch.ExtractKeywords(query, c.tokenizer)
	if len(keywords) == 0 {
		return nil, nil
	}
	allow := allowSet(opts.Whitelist)
	type scored struct {
		summary sdkskill.Summary
		score   float64
	}
	var scoredSkills []scored
	for name, sk := range c.skills {
		if len(allow) > 0 && !allow[name] {
			continue
		}
		score := textsearch.BM25(c.docTokens[name], keywords, c.corpus)
		if score <= 0 {
			continue
		}
		scoredSkills = append(scoredSkills, scored{
			summary: copySummary(sk.Summary),
			score:   score,
		})
	}
	sort.Slice(scoredSkills, func(i, j int) bool {
		if scoredSkills[i].score == scoredSkills[j].score {
			return scoredSkills[i].summary.Name < scoredSkills[j].summary.Name
		}
		return scoredSkills[i].score > scoredSkills[j].score
	})
	limit := opts.Limit
	if limit <= 0 || limit > len(scoredSkills) {
		limit = len(scoredSkills)
	}
	out := make([]sdkskill.Summary, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, scoredSkills[i].summary)
	}
	return out, nil
}

// Load returns a full skill by name.
func (c *Catalog) Load(ctx context.Context, name string) (*sdkskill.Skill, error) {
	if err := c.ensureIndex(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	sk, ok := c.skills[name]
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	return copySkill(sk), nil
}

func allowSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

func searchText(sk *sdkskill.Skill) string {
	return sk.Name + " " + sk.Description + " " + strings.Join(sk.Tags, " ")
}

func evaluateGating(req *sdkskill.Requires) *sdkskill.Gating {
	if req == nil {
		return &sdkskill.Gating{Available: true}
	}
	g := &sdkskill.Gating{Available: true}
	if len(req.OS) > 0 {
		matched := false
		for _, want := range req.OS {
			if strings.EqualFold(want, runtime.GOOS) {
				matched = true
				break
			}
		}
		if !matched {
			return &sdkskill.Gating{
				Available: false,
				Reason:    "unsupported OS: " + runtime.GOOS,
			}
		}
	}
	for _, bin := range req.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			g.MissingBins = append(g.MissingBins, bin)
		}
	}
	if len(req.AnyBins) > 0 {
		found := false
		for _, bin := range req.AnyBins {
			if _, err := exec.LookPath(bin); err == nil {
				found = true
				break
			}
		}
		if !found {
			g.MissingAnyBins = append(g.MissingAnyBins, req.AnyBins...)
		}
	}
	for _, env := range req.Env {
		if _, ok := os.LookupEnv(env); !ok {
			g.MissingEnv = append(g.MissingEnv, env)
		}
	}
	if len(g.MissingBins) > 0 || len(g.MissingAnyBins) > 0 || len(g.MissingEnv) > 0 {
		g.Available = false
	}
	return g
}

func missingDeps(g *sdkskill.Gating) []string {
	if g == nil || g.Available {
		return nil
	}
	var out []string
	out = append(out, g.MissingBins...)
	if len(g.MissingAnyBins) > 0 {
		out = append(out, "one of: "+strings.Join(g.MissingAnyBins, "|"))
	}
	out = append(out, g.MissingEnv...)
	if len(out) == 0 && g.Reason != "" {
		out = append(out, g.Reason)
	}
	return out
}

type frontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Tags        []string       `yaml:"tags"`
	Homepage    string         `yaml:"homepage"`
	Metadata    map[string]any `yaml:"metadata"`
	Requires    map[string]any `yaml:"requires"`
}

func parseSkillMarkdown(content string) (*sdkskill.Skill, error) {
	block, err := extractFrontmatter(content)
	if err != nil {
		return nil, err
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}
	if fm.Name == "" {
		return nil, fmt.Errorf("SKILL.md missing name")
	}
	if fm.Description == "" {
		return nil, fmt.Errorf("SKILL.md missing description")
	}
	requires := requiresFromFrontmatter(fm)
	return &sdkskill.Skill{
		Summary: sdkskill.Summary{
			Name:        fm.Name,
			Description: fm.Description,
			Tags:        copyStrings(fm.Tags),
			Source:      fm.Homepage,
		},
		Body:     content,
		Requires: requires,
	}, nil
}

func extractFrontmatter(content string) (string, error) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", fmt.Errorf("SKILL.md missing frontmatter")
	}
	rest := content[4:]
	before, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", fmt.Errorf("SKILL.md frontmatter not closed")
	}
	return before, nil
}

func requiresFromFrontmatter(fm frontmatter) *sdkskill.Requires {
	if req := mapFromOpenClawMetadata(fm.Metadata); len(req) > 0 {
		return requiresFromMap(req)
	}
	return requiresFromMap(fm.Requires)
}

func mapFromOpenClawMetadata(metadata map[string]any) map[string]any {
	openclaw, ok := metadata["openclaw"].(map[any]any)
	if !ok {
		if typed, ok := metadata["openclaw"].(map[string]any); ok {
			return mapFromAny(typed["requires"])
		}
		return nil
	}
	return mapFromAny(openclaw["requires"])
}

func mapFromAny(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			if s, ok := k.(string); ok {
				out[s] = v
			}
		}
		return out
	default:
		return nil
	}
}

func requiresFromMap(m map[string]any) *sdkskill.Requires {
	if len(m) == 0 {
		return nil
	}
	req := &sdkskill.Requires{
		Bins:    stringSlice(m["bins"]),
		AnyBins: stringSlice(m["any_bins"]),
		Env:     stringSlice(m["env"]),
		OS:      stringSlice(m["os"]),
	}
	if len(req.Bins) == 0 && len(req.AnyBins) == 0 && len(req.Env) == 0 && len(req.OS) == 0 {
		return nil
	}
	return req
}

func stringSlice(v any) []string {
	switch vv := v.(type) {
	case []string:
		return copyStrings(vv)
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func copySkill(in *sdkskill.Skill) *sdkskill.Skill {
	if in == nil {
		return nil
	}
	out := *in
	out.Summary = copySummary(in.Summary)
	if in.Requires != nil {
		req := *in.Requires
		req.Bins = copyStrings(in.Requires.Bins)
		req.AnyBins = copyStrings(in.Requires.AnyBins)
		req.Env = copyStrings(in.Requires.Env)
		req.OS = copyStrings(in.Requires.OS)
		out.Requires = &req
	}
	if in.Gating != nil {
		g := *in.Gating
		g.MissingBins = copyStrings(in.Gating.MissingBins)
		g.MissingAnyBins = copyStrings(in.Gating.MissingAnyBins)
		g.MissingEnv = copyStrings(in.Gating.MissingEnv)
		out.Gating = &g
	}
	return &out
}

func copySummary(in sdkskill.Summary) sdkskill.Summary {
	out := in
	out.Tags = copyStrings(in.Tags)
	out.MissingDeps = copyStrings(in.MissingDeps)
	return out
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

var _ sdkskill.Catalog = (*Catalog)(nil)
