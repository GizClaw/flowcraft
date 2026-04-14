package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"

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

func (s *ScopedWorkspace) Read(ctx context.Context, path string) ([]byte, error) {
	if err := s.checkRead(ctx, path); err != nil {
		return nil, err
	}
	return s.inner.Read(ctx, path)
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

type ScopedGitWorkspace struct {
	*ScopedWorkspace
	git GitWorkspace
}

func NewScopedGitWorkspace(inner GitWorkspace, opts ...ScopedOption) *ScopedGitWorkspace {
	return &ScopedGitWorkspace{
		ScopedWorkspace: NewScopedWorkspace(inner, opts...),
		git:             inner,
	}
}

func (s *ScopedGitWorkspace) GitClone(ctx context.Context, url, dest string) error {
	if err := s.checkWrite(ctx, dest); err != nil {
		return err
	}
	return s.git.GitClone(ctx, url, dest)
}

func (s *ScopedGitWorkspace) GitPull(ctx context.Context, dir string) error {
	if err := s.checkWrite(ctx, dir); err != nil {
		return err
	}
	return s.git.GitPull(ctx, dir)
}

func (s *ScopedGitWorkspace) GitHead(ctx context.Context, dir string) (string, error) {
	if err := s.checkRead(ctx, dir); err != nil {
		return "", err
	}
	return s.git.GitHead(ctx, dir)
}

var _ GitWorkspace = (*ScopedGitWorkspace)(nil)

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
	for _, p := range patterns {
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
			if strings.Contains(suffix, "*") || strings.Contains(suffix, "?") {
				if matched, _ := filepath.Match(suffix, filepath.Base(path)); matched {
					return true
				}
			}
			continue
		}
		if matched, _ := filepath.Match(p, path); matched {
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
