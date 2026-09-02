package workspace

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// ScopedWorkspace wraps a Workspace with dual-mode permission enforcement:
//   - Read: deny-only mode — everything readable unless path matches denyRead
//   - Write: allow-only mode — nothing writable unless path matches allowWrite
//   - Mandatory deny paths are always blocked for both read and write
type ScopedWorkspace struct {
	inner         Workspace
	denyRead      []string
	allowWrite    []string
	mandatoryDeny []string
	logger        ViolationLogger
}

type ScopedOption func(*ScopedWorkspace)

func WithDenyRead(patterns ...string) ScopedOption {
	return func(s *ScopedWorkspace) { s.denyRead = append(s.denyRead, patterns...) }
}

func WithAllowWrite(patterns ...string) ScopedOption {
	return func(s *ScopedWorkspace) { s.allowWrite = append(s.allowWrite, patterns...) }
}

func WithMandatoryDeny(patterns ...string) ScopedOption {
	return func(s *ScopedWorkspace) { s.mandatoryDeny = append(s.mandatoryDeny, patterns...) }
}

func WithViolationLogger(l ViolationLogger) ScopedOption {
	return func(s *ScopedWorkspace) { s.logger = l }
}

func NewScopedWorkspace(inner Workspace, opts ...ScopedOption) *ScopedWorkspace {
	sw := &ScopedWorkspace{inner: inner, logger: defaultViolationLogger{}}
	for _, o := range opts {
		o(sw)
	}
	return sw
}

// Close forwards to the wrapped workspace when it owns closable
// resources (e.g. *LocalWorkspace). This lets the deploy assembly close
// a scoped local workspace deterministically.
func (s *ScopedWorkspace) Close() error {
	if s == nil || s.inner == nil {
		return nil
	}
	if closer, ok := s.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Unwrap returns the wrapped Workspace. It lets callers recover
// implementation-specific extras (e.g. *LocalWorkspace.Root) that
// the scoped wrapper does not forward — at the caller's own risk,
// since operations on the unwrapped workspace bypass this scope's
// permission policy.
func (s *ScopedWorkspace) Unwrap() Workspace {
	return s.inner
}

// Capabilities forwards to the wrapped Workspace, since scoping
// is a security boundary that does not change durability /
// atomicity / consistency / distribution semantics. A wrapped
// Workspace that does not implement [CapabilityReporter] yields a
// zero-value (all-false) [Capabilities] via [CapabilitiesOf].
func (s *ScopedWorkspace) Capabilities() Capabilities {
	return CapabilitiesOf(s.inner)
}

func (s *ScopedWorkspace) Read(ctx context.Context, path string) ([]byte, error) {
	if err := s.checkRead(ctx, path); err != nil {
		return nil, err
	}
	return s.inner.Read(ctx, path)
}

// ReadLimited forwards to the wrapped workspace when it supports
// bounded reads; otherwise the operation is unavailable so callers
// fail closed instead of falling back to an unbounded materialization.
func (s *ScopedWorkspace) ReadLimited(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if err := s.checkRead(ctx, path); err != nil {
		return nil, err
	}
	lr, ok := s.inner.(LimitedReader)
	if !ok {
		return nil, errdefs.NotAvailablef("workspace: scoped workspace cannot perform bounded reads")
	}
	return lr.ReadLimited(ctx, path, maxBytes)
}

func (s *ScopedWorkspace) Write(ctx context.Context, path string, data []byte) error {
	if err := s.checkWrite(ctx, path); err != nil {
		return err
	}
	return s.inner.Write(ctx, path, data)
}

func (s *ScopedWorkspace) Append(ctx context.Context, path string, data []byte) error {
	if err := s.checkWrite(ctx, path); err != nil {
		return err
	}
	return s.inner.Append(ctx, path, data)
}

func (s *ScopedWorkspace) Rename(ctx context.Context, src, dst string) error {
	if err := s.checkWrite(ctx, src); err != nil {
		return err
	}
	if err := s.checkWrite(ctx, dst); err != nil {
		return err
	}
	return s.inner.Rename(ctx, src, dst)
}

func (s *ScopedWorkspace) Delete(ctx context.Context, path string) error {
	if err := s.checkWrite(ctx, path); err != nil {
		return err
	}
	return s.inner.Delete(ctx, path)
}

func (s *ScopedWorkspace) RemoveAll(ctx context.Context, path string) error {
	if err := s.checkWrite(ctx, path); err != nil {
		return err
	}
	return s.inner.RemoveAll(ctx, path)
}

func (s *ScopedWorkspace) List(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	if err := s.checkRead(ctx, dir); err != nil {
		return nil, err
	}
	return s.inner.List(ctx, dir)
}

func (s *ScopedWorkspace) Exists(ctx context.Context, path string) (bool, error) {
	if err := s.checkRead(ctx, path); err != nil {
		return false, err
	}
	return s.inner.Exists(ctx, path)
}

func (s *ScopedWorkspace) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	if err := s.checkRead(ctx, path); err != nil {
		return nil, err
	}
	return s.inner.Stat(ctx, path)
}

func (s *ScopedWorkspace) checkRead(ctx context.Context, path string) error {
	cleaned := filepath.Clean(path)
	if matchesAny(cleaned, s.mandatoryDeny) {
		s.logViolation(ctx, "read", path, "mandatory deny")
		return fmt.Errorf("%w: %s (mandatory deny)", ErrAccessDenied, path)
	}
	if matchesAny(cleaned, s.denyRead) {
		s.logViolation(ctx, "read", path, "deny-read pattern matched")
		return fmt.Errorf("%w: %s (read denied)", ErrAccessDenied, path)
	}
	return nil
}

func (s *ScopedWorkspace) checkWrite(ctx context.Context, path string) error {
	cleaned := filepath.Clean(path)
	if matchesAny(cleaned, s.mandatoryDeny) {
		s.logViolation(ctx, "write", path, "mandatory deny")
		return fmt.Errorf("%w: %s (mandatory deny)", ErrAccessDenied, path)
	}
	if !matchesAny(cleaned, s.allowWrite) {
		s.logViolation(ctx, "write", path, "not in allow-write list")
		return fmt.Errorf("%w: %s (write denied)", ErrAccessDenied, path)
	}
	return nil
}

func (s *ScopedWorkspace) logViolation(ctx context.Context, op, path, reason string) {
	if s.logger != nil {
		s.logger.LogViolation(ctx, ViolationRecord{
			Time:      time.Now(),
			Operation: op,
			Path:      path,
			Reason:    reason,
		})
	}
}

// matchesAny reports whether path matches any of the given patterns.
//
// Supported pattern forms:
//   - "dir/**"       — matches dir itself and everything under dir/
//   - "**/dir/**"    — matches dir at any depth and everything under it
//   - "**/name"      — matches name at any depth (file or directory)
//   - "*.ext"        — matches only in the same directory (filepath.Match on full path)
//
// To match a file extension at any depth, use the explicit "**/*.ext" form.
func matchesAny(path string, patterns []string) bool {
	path = foldCase(filepath.ToSlash(path))
	for _, p := range patterns {
		p = foldCase(filepath.ToSlash(p))
		if strings.HasSuffix(p, "/**") {
			prefix := strings.TrimSuffix(p, "/**")
			if strings.HasPrefix(prefix, "**/") {
				dir := strings.TrimPrefix(prefix, "**/")
				if path == dir || strings.HasPrefix(path, dir+"/") ||
					strings.Contains(path, "/"+dir+"/") || strings.HasSuffix(path, "/"+dir) {
					return true
				}
				continue
			}
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
			continue
		}
		if strings.HasPrefix(p, "**/") {
			suffix := strings.TrimPrefix(p, "**/")
			if path == suffix || strings.HasSuffix(path, "/"+suffix) {
				return true
			}
			if strings.ContainsAny(suffix, "*?") {
				if matched, _ := pathpkg.Match(suffix, pathpkg.Base(path)); matched {
					return true
				}
			}
			continue
		}
		if matched, _ := pathpkg.Match(p, path); matched {
			return true
		}
	}
	return false
}

type defaultViolationLogger struct{}

func (defaultViolationLogger) LogViolation(ctx context.Context, r ViolationRecord) {
	telemetry.Warn(ctx, "workspace: access violation",
		otellog.String("op", r.Operation),
		otellog.String("path", r.Path),
		otellog.String("reason", r.Reason))
}
