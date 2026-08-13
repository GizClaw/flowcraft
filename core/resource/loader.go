package resource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

const defaultMaxBytes = 1 << 20

// Loader materializes [Source] values. File references resolve against
// the loader base dir and may not escape it (lexically or through
// symlinks); embed references resolve against an injected fs.FS; both
// are capped at MaxBytes.
type Loader struct {
	baseDir  string
	embed    fs.FS
	maxBytes int64
}

// LoaderOption configures a Loader.
type LoaderOption func(*Loader)

// WithBaseDir sets the base directory file references resolve against.
func WithBaseDir(dir string) LoaderOption {
	return func(l *Loader) { l.baseDir = dir }
}

// WithEmbed supplies the fs.FS embed references resolve against.
func WithEmbed(fsys fs.FS) LoaderOption {
	return func(l *Loader) { l.embed = fsys }
}

// WithMaxBytes caps materialized content. Zero keeps the default
// (1 MiB); a negative value disables the cap.
func WithMaxBytes(n int64) LoaderOption {
	return func(l *Loader) { l.maxBytes = n }
}

// NewLoader returns a Loader with the given options.
func NewLoader(opts ...LoaderOption) *Loader {
	loader := &Loader{maxBytes: defaultMaxBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(loader)
		}
	}
	return loader
}

// BaseDir returns the directory file references resolve against, or ""
// when no base dir is configured.
func (l *Loader) BaseDir() string {
	if l == nil {
		return ""
	}
	return l.baseDir
}

// Load materializes src: inline content is returned unchanged, file
// and embed references are read and size-capped.
func (l *Loader) Load(ctx context.Context, src Source) ([]byte, error) {
	switch {
	case src.File != "":
		return l.loadFile(ctx, src.File)
	case src.Embed != "":
		return l.loadEmbed(ctx, src.Embed)
	default:
		return bytes.Clone(src.Inline), nil
	}
}

func (l *Loader) loadFile(_ context.Context, name string) ([]byte, error) {
	if l.baseDir == "" {
		return nil, errdefs.Validationf(
			"resource loader: file source requires a base dir")
	}
	if filepath.IsAbs(name) {
		return nil, errdefs.Validationf(
			"resource loader: file source %q must be relative", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(
		clean, ".."+string(filepath.Separator)) {
		return nil, errdefs.Forbiddenf(
			"resource loader: file source %q escapes base dir", name)
	}
	root, err := os.OpenRoot(l.baseDir)
	if err != nil {
		return nil, errdefs.Forbiddenf(
			"resource loader: open base dir: %v", err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errdefs.NotFoundf(
				"resource loader: file %q not found", name)
		}
		return nil, errdefs.Forbiddenf(
			"resource loader: read file %q: %v", name, err)
	}
	if l.maxBytes >= 0 && int64(len(data)) > l.maxBytes {
		return nil, errdefs.Validationf(
			"resource loader: file %q exceeds %d bytes", name, l.maxBytes)
	}
	return data, nil
}

func (l *Loader) loadEmbed(_ context.Context, name string) ([]byte, error) {
	if l.embed == nil {
		return nil, errdefs.Validationf(
			"resource loader: embed source requires an embed FS")
	}
	if !fs.ValidPath(name) {
		return nil, errdefs.Validationf(
			"resource loader: invalid embed path %q", name)
	}
	data, err := fs.ReadFile(l.embed, name)
	if err != nil {
		return nil, errdefs.NotFoundf(
			"resource loader: embed %q: %v", name, err)
	}
	if l.maxBytes >= 0 && int64(len(data)) > l.maxBytes {
		return nil, errdefs.Validationf(
			"resource loader: embed %q exceeds %d bytes", name, l.maxBytes)
	}
	return data, nil
}

// Describe returns a human-readable description of the source for
// errors and provenance.
func (s Source) Describe() string {
	switch {
	case s.File != "":
		return fmt.Sprintf("file:%s", s.File)
	case s.Embed != "":
		return fmt.Sprintf("embed:%s", s.Embed)
	default:
		return "inline"
	}
}
