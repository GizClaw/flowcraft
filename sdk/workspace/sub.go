package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type subWorkspace struct {
	inner   Workspace
	prefix  string
	initErr error
}

var _ Workspace = (*subWorkspace)(nil)

// Sub returns a Workspace view rooted under prefix.
//
// All operations are forwarded to the parent workspace after prefixing the
// caller's path. For local workspaces, the returned value also exposes Root()
// so path-backed embedded stores can open files inside the subdirectory.
func Sub(inner Workspace, prefix string) Workspace {
	if inner == nil {
		return nil
	}
	cleaned, err := cleanPath(prefix)
	if err != nil {
		return &subWorkspace{inner: inner, initErr: fmt.Errorf("workspace sub: invalid prefix %q: %w", prefix, err)}
	}
	if cleaned == "" {
		return inner
	}
	switch current := inner.(type) {
	case *subWorkspace:
		if current.initErr != nil {
			return &subWorkspace{inner: current.inner, initErr: current.initErr}
		}
		inner = current.inner
		cleaned = filepath.Join(current.prefix, cleaned)
	}
	sw := &subWorkspace{inner: inner, prefix: cleaned}
	switch typed := inner.(type) {
	case *LocalWorkspace:
		local, err := typed.Sub(cleaned)
		if err != nil {
			return &subWorkspace{inner: inner, initErr: fmt.Errorf("workspace sub: open local root %q: %w", cleaned, err)}
		}
		return local
	}
	return sw
}

func (s *subWorkspace) Capabilities() Capabilities {
	return CapabilitiesOf(s.inner)
}

func (s *subWorkspace) Read(ctx context.Context, path string) ([]byte, error) {
	p, err := s.join(path)
	if err != nil {
		return nil, err
	}
	return s.inner.Read(ctx, p)
}

func (s *subWorkspace) Write(ctx context.Context, path string, data []byte) error {
	p, err := s.join(path)
	if err != nil {
		return err
	}
	return s.inner.Write(ctx, p, data)
}

func (s *subWorkspace) Append(ctx context.Context, path string, data []byte) error {
	p, err := s.join(path)
	if err != nil {
		return err
	}
	return s.inner.Append(ctx, p, data)
}

func (s *subWorkspace) Rename(ctx context.Context, src, dst string) error {
	srcPath, err := s.join(src)
	if err != nil {
		return err
	}
	dstPath, err := s.join(dst)
	if err != nil {
		return err
	}
	return s.inner.Rename(ctx, srcPath, dstPath)
}

func (s *subWorkspace) Delete(ctx context.Context, path string) error {
	p, err := s.join(path)
	if err != nil {
		return err
	}
	return s.inner.Delete(ctx, p)
}

func (s *subWorkspace) RemoveAll(ctx context.Context, path string) error {
	if p, err := cleanPath(path); err != nil {
		return err
	} else if p == "" {
		return errdefs.Forbiddenf("workspace: refusing to remove root")
	}
	p, err := s.join(path)
	if err != nil {
		return err
	}
	return s.inner.RemoveAll(ctx, p)
}

func (s *subWorkspace) List(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	p, err := s.join(dir)
	if err != nil {
		return nil, err
	}
	return s.inner.List(ctx, p)
}

func (s *subWorkspace) Exists(ctx context.Context, path string) (bool, error) {
	p, err := s.join(path)
	if err != nil {
		return false, err
	}
	return s.inner.Exists(ctx, p)
}

func (s *subWorkspace) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	p, err := s.join(path)
	if err != nil {
		return nil, err
	}
	return s.inner.Stat(ctx, p)
}

func (s *subWorkspace) join(path string) (string, error) {
	if s.initErr != nil {
		return "", s.initErr
	}
	cleaned, err := cleanPath(path)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		return s.prefix, nil
	}
	return filepath.Join(s.prefix, cleaned), nil
}
