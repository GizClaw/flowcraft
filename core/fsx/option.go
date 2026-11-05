package fsx

import (
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
)

// Option configures a filesystem implementation.
type Option func(*options)

type options struct {
	writable []string
}

// WithWritable declares the absolute paths a filesystem may write. Paths are
// subtree-based, exactly like a mount: /w covers /w and everything beneath it.
//
// These are the instance defaults. A call that carries its own non-empty
// permission policy replaces them for that call, which is how a narrower view
// (a subagent, a single tool) is expressed without building a second
// filesystem.
func WithWritable(paths ...string) Option {
	return func(o *options) { o.writable = append(o.writable, paths...) }
}

func buildOptions(opts []Option) (*options, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	for i, path := range o.writable {
		if !filepath.IsAbs(path) {
			return nil, errdefs.Validationf(
				"fsx: writable path %q must be absolute", path)
		}
		o.writable[i] = filepath.Clean(path)
	}
	return o, nil
}

// writable reports whether path may be written under this call's boundary.
//
// The per-call policy wins when it declares entries at all; otherwise the
// instance defaults apply, and an instance with no declaration is writable
// across its whole root (the local-directory default, matching what a
// workspace directory means to a host application).
func writable(path string, sb *Sandbox, defaults []string) bool {
	if sb != nil && len(sb.Policy.Files.Entries) > 0 {
		return sb.Policy.Files.Allows(path, permission.Write)
	}
	for _, root := range defaults {
		if permission.Within(root, path) {
			return true
		}
	}
	return false
}

// readable reports whether path may be read under this call's boundary.
//
// Read entries are a tool-layer guard: no sandbox backend can project them, so
// a spawned command can still read a denied path. They are enforced here, by
// the interpreter that performs the read, and the documentation says so rather
// than implying a boundary the kernel does not provide.
func readable(path string, sb *Sandbox) bool {
	if sb == nil || len(sb.Policy.Files.Entries) == 0 {
		return true
	}
	return sb.Policy.Files.Allows(path, permission.Read)
}
