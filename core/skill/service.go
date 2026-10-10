package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/utils/bm25"
)

// defaultTopN caps a ranked list when neither the call nor Options
// sets a limit.
const defaultTopN = 5

// Options configures a Service.
type Options struct {
	// Roots are the directories discovery scans (see [Discover]).
	// Compose them at the call site: the conventional personal root
	// ~/.agents/skills is not implicit.
	Roots []string
	// TopN caps ranked results when a call passes no limit of its own.
	// Defaults to 5.
	TopN int
	// MinScore is the default BM25 acceptance threshold. Zero accepts
	// any match.
	MinScore float64
	// Disabled lists skill names, or absolute SKILL.md paths from the
	// roots, excluded from the registry and therefore from ranking.
	Disabled []string
}

// Service is a shared skills registry: one discovery pass, one BM25
// index over skill names and descriptions, plus read access to
// SKILL.md bodies. The registry is an immutable snapshot under an
// atomic pointer, so [Service.Reload] swaps in a freshly discovered
// registry without locking the hot read paths (per-turn ranking,
// mention resolution, rendering).
//
// Shadowing is the caller's policy. Duplicate names across roots are
// all listed and rankable; [Service.ByName] and [Service.Mentioned]
// resolve the first path in the canonical (name, path) order.
type Service struct {
	ctx      context.Context
	opts     Options
	snapshot atomic.Pointer[snapshot]
}

// snapshot is one immutable discovery result.
type snapshot struct {
	outcome   Outcome
	byPath    map[string]Metadata
	index     *bm25.Index
	scanRoots []string // symlink-resolved roots for body-read containment
}

// NewService discovers skills and builds the shared index. Roots are
// scanned once here and again on every [Service.Reload].
func NewService(ctx context.Context, opts Options) *Service {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.TopN <= 0 {
		opts.TopN = defaultTopN
	}
	s := &Service{ctx: ctx, opts: opts}
	s.reload()
	return s
}

// Reload re-runs discovery and swaps in a fresh snapshot, so skills
// added on disk become visible without rebuilding the service.
func (s *Service) Reload() { s.reload() }

func (s *Service) reload() {
	outcome := Discover(s.ctx, s.opts.Roots)
	outcome.Skills = filterDisabled(outcome.Skills, s.opts.Disabled)
	byPath := make(map[string]Metadata, len(outcome.Skills))
	docs := make([]bm25.Doc, 0, len(outcome.Skills))
	for _, sk := range outcome.Skills {
		byPath[sk.Path] = sk
		docs = append(docs, bm25.Doc{ID: sk.Path, Name: sk.Name, Text: sk.Description})
	}
	// Skills arrive sorted by (name, path) and the kernel keeps the
	// document order for score ties, so equal scores stay stable.
	index, err := bm25.New(docs)
	if err != nil {
		// bm25.New rejects duplicate IDs; discovery de-duplicates
		// canonical paths, so this cannot happen. Degrade to an
		// unranked registry rather than losing the skills entirely.
		telemetry.WarnErr(s.ctx, "skill: build rank index failed", err)
	}
	s.snapshot.Store(&snapshot{
		outcome:   outcome,
		byPath:    byPath,
		index:     index,
		scanRoots: cleanedRoots(outcome.ScanRoots),
	})
}

// filterDisabled returns the skills that survive the disabled list: an
// absolute path matches the canonical path exactly, anything else
// matches the name. The input slice is left untouched — callers keep
// it in the snapshot.
func filterDisabled(skills []Metadata, disabled []string) []Metadata {
	if len(disabled) == 0 {
		return skills
	}
	names := map[string]bool{}
	paths := map[string]bool{}
	for _, d := range disabled {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if filepath.IsAbs(d) {
			paths[filepath.Clean(d)] = true
		} else {
			names[d] = true
		}
	}
	if len(names) == 0 && len(paths) == 0 {
		return skills
	}
	out := make([]Metadata, 0, len(skills))
	for _, sk := range skills {
		if names[sk.Name] || paths[filepath.Clean(sk.Path)] {
			continue
		}
		out = append(out, sk)
	}
	return out
}

// TopN returns the configured ranked-list size.
func (s *Service) TopN() int { return s.opts.TopN }

// MinScore returns the configured BM25 threshold.
func (s *Service) MinScore() float64 { return s.opts.MinScore }

// List returns all discovered skills sorted by name, then path. It is
// deliberately unfiltered beyond Options.Disabled: shadowing and
// lifecycle decisions belong to the caller, which sees every path.
func (s *Service) List() []Metadata {
	snap := s.snapshot.Load()
	out := append([]Metadata(nil), snap.outcome.Skills...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Diagnostics returns the non-fatal diagnostics of the last discovery.
func (s *Service) Diagnostics() []Diagnostic {
	snap := s.snapshot.Load()
	return append([]Diagnostic(nil), snap.outcome.Diagnostics...)
}

// Roots returns the scan roots that contained at least one skill.
func (s *Service) Roots() []string {
	snap := s.snapshot.Load()
	return append([]string(nil), snap.outcome.Roots...)
}

// ByName resolves a skill by name. Duplicate names across roots are
// all discovered; ByName picks the first path in the canonical
// (name, path) order, the same order [Service.List] returns.
func (s *Service) ByName(name string) (Metadata, bool) {
	snap := s.snapshot.Load()
	for _, sk := range snap.outcome.Skills {
		if sk.Name == name {
			return sk, true
		}
	}
	return Metadata{}, false
}

// ReadFull returns a skill's metadata plus its full SKILL.md body,
// with the frontmatter stripped (the activation and skill_read paths
// share this).
func (s *Service) ReadFull(name string) (Metadata, string, error) {
	sk, ok := s.ByName(name)
	if !ok {
		return Metadata{}, "", errdefs.NotFoundf("skill: %q not found", name)
	}
	body, err := s.readBody(sk)
	if err != nil {
		return Metadata{}, "", err
	}
	return sk, body, nil
}

// ReadByPath returns the metadata plus full SKILL.md body for one
// discovered skill, identified by the exact path [Service.List]
// returned. Like ReadFull it only serves paths in the current
// snapshot, so a stale path from a UI cannot read an arbitrary file
// on disk.
func (s *Service) ReadByPath(path string) (Metadata, string, error) {
	snap := s.snapshot.Load()
	sk, ok := snap.byPath[path]
	if !ok {
		return Metadata{}, "", errdefs.NotFoundf("skill: %q not found", path)
	}
	body, err := s.readBody(sk)
	if err != nil {
		return Metadata{}, "", err
	}
	return sk, body, nil
}

// readBody re-reads one SKILL.md through the snapshot's roots: the
// file is resolved again, refused when it left the roots after
// discovery, size-capped again, and returned with frontmatter
// stripped. The read opens the resolved path, not the discovered one,
// so only a swap of the resolved file itself can race the containment
// check — accepted: the roots are trusted content directories, and a
// writer able to swap files inside them already decides what the
// model reads.
func (s *Service) readBody(sk Metadata) (string, error) {
	snap := s.snapshot.Load()
	resolved, err := filepath.EvalSymlinks(sk.Path)
	if err != nil {
		return "", fmt.Errorf("skill: resolve %s: %w", sk.Path, err)
	}
	if !insideAnyRoot(resolved, snap.scanRoots) {
		return "", fmt.Errorf(
			"skill: %q resolves outside the configured skill roots", sk.Path)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("skill: read %s: %w", sk.Path, err)
	}
	if len(data) > maxSkillFileBytes {
		return "", fmt.Errorf(
			"skill: %s exceeds the %d-byte SKILL.md limit",
			sk.Path, maxSkillFileBytes)
	}
	_, body, err := splitFrontmatter(data)
	if err != nil {
		return "", fmt.Errorf("skill: %s: %w", sk.Path, err)
	}
	return strings.TrimSpace(string(body)), nil
}
