package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Script is one loaded source.
type Script struct {
	// Name is the reference a person reads in an error: the path, or whatever
	// the host calls it.
	Name string
	// Text is the source itself.
	Text string
	// Version identifies the content. The node never interprets it: it is a
	// cache key, a record field and, for a pinned node, the thing a restart has
	// to find again. A content hash is the useful default.
	Version string
}

// Source resolves a script reference to its source.
//
// It is an interface because where a script lives is a host decision: a file in
// the workspace, an embedded unit, a row in a table. The node only requires that
// a reference resolves to text and a version.
type Source interface {
	Load(ctx context.Context, ref string) (Script, error)
}

// FileSource reads scripts from disk.
//
// The version is the content hash, so editing a file changes the cache key and
// nothing else. A TTL bounds how often a file is stat'ed: reading it on every
// invocation would make hot reload a file-system poll in the step path, and
// caching it forever would make the reload this type exists for never happen.
type FileSource struct {
	// BaseDir resolves a relative reference. Empty means the process working
	// directory, which a host should avoid: it makes a document's meaning depend
	// on where the process was started.
	BaseDir string
	// TTL bounds the stat interval. Zero re-reads the file on every Load.
	TTL time.Duration

	mu      sync.Mutex
	entries map[string]*fileEntry
}

type fileEntry struct {
	script  Script
	mtime   time.Time
	size    int64
	checked time.Time
}

var _ Source = (*FileSource)(nil)

// Load implements [Source].
func (s *FileSource) Load(ctx context.Context, ref string) (Script, error) {
	if strings.TrimSpace(ref) == "" {
		return Script{}, errdefs.Validationf("script: source reference is empty")
	}
	if err := ctx.Err(); err != nil {
		return Script{}, err
	}
	path := ref
	if !filepath.IsAbs(path) && s.BaseDir != "" {
		path = filepath.Join(s.BaseDir, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Script{}, errdefs.NotFoundf("script: %s does not exist", path)
		}
		return Script{}, errdefs.Validationf("script: stat %s: %w", path, err)
	}
	if info.IsDir() {
		return Script{}, errdefs.Validationf("script: %s is a directory", path)
	}

	now := time.Now()
	s.mu.Lock()
	cached := s.entries[path]
	s.mu.Unlock()
	if cached != nil && s.TTL > 0 && now.Sub(cached.checked) < s.TTL {
		return cached.script, nil
	}
	if cached != nil && cached.mtime.Equal(info.ModTime()) && cached.size == info.Size() {
		s.mu.Lock()
		if s.entries == nil {
			s.entries = make(map[string]*fileEntry)
		}
		cached.checked = now
		s.entries[path] = cached
		s.mu.Unlock()
		return cached.script, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Script{}, errdefs.NotFoundf("script: %s does not exist", path)
		}
		return Script{}, errdefs.Validationf("script: read %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	script := Script{
		Name:    path,
		Text:    string(body),
		Version: "sha256:" + hex.EncodeToString(sum[:]),
	}
	s.mu.Lock()
	if s.entries == nil {
		s.entries = make(map[string]*fileEntry)
	}
	s.entries[path] = &fileEntry{
		script:  script,
		mtime:   info.ModTime(),
		size:    info.Size(),
		checked: now,
	}
	s.mu.Unlock()
	return script, nil
}
