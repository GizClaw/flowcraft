package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// defaultMaxBytes bounds a single resolved document. It matches the
// graph definition limit and Kubernetes' ConfigMap ceiling.
const defaultMaxBytes = 1 << 20

// Loader resolves [Source] and [Ref] values into bytes at build time.
// It is the single place path constraints, size limits, and error
// classification live: module parsers receive bytes and never touch the
// filesystem, and the kernel never sees a reference.
//
// A Loader is host-owned and per-host: two Loaders share no state, so
// one host embedding a different asset registry cannot affect another.
type Loader struct {
	baseDir  string
	maxBytes int64
	embed    fs.FS
}

// Option configures a Loader.
type Option func(*Loader)

// WithBaseDir sets the directory file references resolve against.
// Relative references are joined onto it, and after cleaning and
// symlink resolution every target must still land inside it. It
// defaults to ".", matching today's working-directory-relative reads.
func WithBaseDir(dir string) Option {
	return func(l *Loader) {
		if dir != "" {
			l.baseDir = dir
		}
	}
}

// WithMaxBytes bounds every resolved document — literal, file, and
// embed alike. It defaults to 1 MiB.
func WithMaxBytes(n int64) Option {
	return func(l *Loader) {
		if n > 0 {
			l.maxBytes = n
		}
	}
}

// WithEmbed registers the build-time embedded-asset registry (for
// example a go:embed fs.FS or a test fstest.MapFS). Embed references
// resolve through it; without it, embed references fail with a
// validation error.
func WithEmbed(fsys fs.FS) Option {
	return func(l *Loader) {
		if fsys != nil {
			l.embed = fsys
		}
	}
}

// NewLoader returns a Loader with the given options applied over the
// defaults (baseDir ".", maxBytes 1 MiB, no embed registry).
func NewLoader(opts ...Option) *Loader {
	l := &Loader{baseDir: ".", maxBytes: defaultMaxBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	return l
}

// Load resolves a Source to bytes. Literal content is returned as-is
// (bounded by maxBytes); file and embed references are resolved and
// read with the same bound. The zero Source (an absent or empty field)
// is a validation error, mirroring the old "file or inline is
// required" contract.
func (l *Loader) Load(ctx context.Context, src Source) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch src.kind {
	case SourceLiteral:
		if src.literal == "" {
			return nil, errdefs.Validationf(
				"config source: literal document is empty")
		}
		if int64(len(src.literal)) > l.maxBytes {
			return nil, errdefs.Validationf(
				"config source: literal exceeds %d bytes", l.maxBytes)
		}
		return []byte(src.literal), nil
	case SourceContent:
		if len(src.raw) == 0 || string(bytes.TrimSpace(src.raw)) == "null" {
			return nil, errdefs.Validationf(
				"config source: content document is empty")
		}
		if int64(len(src.raw)) > l.maxBytes {
			return nil, errdefs.Validationf(
				"config source: content exceeds %d bytes", l.maxBytes)
		}
		return src.raw, nil
	case SourceFile:
		return l.loadFile(ctx, src.path)
	case SourceEmbed:
		return l.loadEmbed(ctx, src.path)
	default:
		return nil, errdefs.Validationf(
			"config source: file or embed is required")
	}
}

// Resolve resolves a Ref (file or embed) to bytes. It shares the same
// path constraints, size bound, and error classification as [Loader.Load].
func (l *Loader) Resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch ref.kind {
	case SourceFile:
		return l.loadFile(ctx, ref.path)
	case SourceEmbed:
		return l.loadEmbed(ctx, ref.path)
	default:
		return nil, errdefs.Validationf(
			"config ref: file or embed is required")
	}
}

// confine resolves name against baseDir and verifies the cleaned,
// symlink-resolved target stays inside the resolved base directory —
// the same model the file secret resolver uses. Absolute references
// are allowed as long as they land inside baseDir; anything escaping
// it is forbidden.
func (l *Loader) confine(name string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(l.baseDir))
	if err != nil {
		return "", errdefs.Internalf(
			"config source: resolve base directory: %v", err)
	}
	if evaluated, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		root = evaluated
	}
	target := name
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target = filepath.Clean(target)
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", errdefs.Internalf(
			"config source: resolve %q: %v", name, err)
	}
	if evaluated, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = evaluated
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", errdefs.Forbiddenf(
			"config source: %q escapes base directory", name)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errdefs.Forbiddenf(
			"config source: %q escapes base directory", name)
	}
	return abs, nil
}

func (l *Loader) loadFile(ctx context.Context, name string) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errdefs.Validationf(
			"config source: file path is required")
	}
	path, err := l.confine(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, errdefs.NotFound(fmt.Errorf(
				"config source: read %q: %w", path, err))
		case errors.Is(err, fs.ErrPermission):
			return nil, errdefs.Forbidden(fmt.Errorf(
				"config source: read %q: %w", path, err))
		default:
			return nil, errdefs.Validation(fmt.Errorf(
				"config source: read %q: %w", path, err))
		}
	}
	defer func() { _ = file.Close() }()
	if err := requireRegularFile(file, path); err != nil {
		return nil, errdefs.Validationf(
			"config source: %q is not a regular file", path)
	}
	data, err := l.readBounded(file, path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func (l *Loader) loadEmbed(ctx context.Context, name string) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errdefs.Validationf(
			"config source: embed name is required")
	}
	if l.embed == nil {
		return nil, errdefs.Validationf(
			"config source: embed registry is not configured")
	}
	name = filepath.ToSlash(name)
	if !fs.ValidPath(name) {
		return nil, errdefs.Validationf(
			"config source: invalid embed path %q", name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := l.embed.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errdefs.NotFound(fmt.Errorf(
				"config source: embed %q: %w", name, err))
		}
		return nil, errdefs.Validation(fmt.Errorf(
			"config source: embed %q: %w", name, err))
	}
	defer func() { _ = file.Close() }()
	if err := requireRegularFile(file, name); err != nil {
		return nil, errdefs.Validationf(
			"config source: embed %q is not a regular file", name)
	}
	data, err := l.readBounded(file, name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

// readBounded reads at most maxBytes+1 bytes so an oversized document
// is detected and rejected instead of silently truncated.
func (l *Loader) readBounded(reader io.Reader, path string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, l.maxBytes+1))
	if err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"config source: read %q: %w", path, err))
	}
	if int64(len(data)) > l.maxBytes {
		return nil, errdefs.Validationf(
			"config source: %q exceeds %d bytes", path, l.maxBytes)
	}
	return data, nil
}

func requireRegularFile(file fs.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &fs.PathError{Op: "read", Path: path, Err: fs.ErrInvalid}
	}
	return nil
}
