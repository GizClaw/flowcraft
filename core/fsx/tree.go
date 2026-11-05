package fsx

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// Runner starts a helper inside a sandbox. It is satisfied by
// sandbox.Runner, so a host passes the same runner that executes commands; a
// test may pass a fake.
type Runner interface {
	Start(ctx context.Context, spec sandbox.SessionSpec) (sandbox.Session, error)
}

// Option configures a Tree.
type Option func(*config)

type config struct {
	writes     []string
	box        Runner
	unenforced bool
	helper     string
}

// WithWritable declares the absolute paths writes are allowed to touch. Paths
// are subtree-based: /w covers /w and everything beneath it.
//
// These are projected onto the sandbox, so the kernel — not this package —
// decides whether a write is inside the set.
func WithWritable(paths ...string) Option {
	return func(c *config) { c.writes = append(c.writes, paths...) }
}

// WithSandbox attaches the runner that performs writes. Without it, Open
// refuses a Tree that claims writable paths, because a writable set that
// nothing enforces is a comment, not a boundary.
func WithSandbox(r Runner) Option {
	return func(c *config) { c.box = r }
}

// WithHelper overrides the executable the sandbox runs for a write. It defaults
// to this process, which is almost always right: the binary is already on the
// host, so re-executing it needs no extra deployment.
//
// Set it when the default path is not visible inside the sandbox — a test
// binary under a private /tmp tmpfs, or a deployment that ships a dedicated
// helper next to the host.
func WithHelper(path string) Option {
	return func(c *config) { c.helper = path }
}

// Unenforced opts into writing without a sandbox: writes go straight to the
// filesystem and the writable set is documentation.
//
// It exists for hosts that have no sandbox at all (a local demo, a test). It is
// deliberately explicit, because "no boundary" must be a decision someone
// wrote down, not a default that happens when a runner is forgotten.
func Unenforced() Option {
	return func(c *config) { c.unenforced = true }
}

// Tree is a file boundary: unrestricted reads, kernel-enforced writes.
type Tree struct {
	writes     []string
	box        Runner
	unenforced bool
	helper     string
}

// Open builds a Tree from options.
//
// It fails when writable paths are declared without either a sandbox or
// Unenforced, and it validates the paths at construction: a relative path or a
// glob can never be projected onto a mount, so discovering that on the first
// write would be too late.
func Open(opts ...Option) (*Tree, error) {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	writes := cleanWrites(c.writes)
	if err := validateWrites(writes); err != nil {
		return nil, err
	}
	if len(writes) > 0 && c.box == nil && !c.unenforced {
		return nil, errdefs.NotAvailablef(
			"fsx: writable paths need a sandbox to enforce them; pass WithSandbox or Unenforced")
	}
	helper := c.helper
	if helper == "" {
		if exe, err := executable(); err == nil {
			helper = exe
		}
	}
	return &Tree{writes: writes, box: c.box, unenforced: c.unenforced, helper: helper}, nil
}

// Writable returns the paths this tree may write.
func (t *Tree) Writable() []string {
	out := make([]string, len(t.writes))
	copy(out, t.writes)
	return out
}

// Enforced reports whether writes are performed by a sandboxed helper.
func (t *Tree) Enforced() bool { return !t.unenforced }

func absolute(op, path string) error {
	if path == "" {
		return errdefs.Validationf("fsx: %s: empty path", op)
	}
	if !isAbs(path) {
		return errdefs.Validationf("fsx: %s: path %q must be absolute", op, path)
	}
	return nil
}

func (t *Tree) checkRead(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}
	return absolute("read", path)
}

// call performs a write operation: through the sandbox when enforced, in
// process when the host opted out.
func (t *Tree) call(ctx context.Context, req helperRequest) error {
	if err := ctx.Err(); err != nil {
		return errdefs.FromContext(err)
	}
	if err := absolute(req.Op, req.Path); err != nil {
		return err
	}
	if t.unenforced {
		return runHelperRequest(req).err()
	}
	return t.runSandboxed(ctx, req)
}

// runSandboxed re-executes this binary as the filesystem helper inside the
// sandbox and speaks one JSON request to it.
func (t *Tree) runSandboxed(ctx context.Context, req helperRequest) error {
	if t.helper == "" {
		return fmt.Errorf("fsx: no helper binary: pass WithHelper or call from a process that has one")
	}
	payload, err := encodeRequest(req)
	if err != nil {
		return err
	}

	sess, err := t.box.Start(ctx, sandbox.SessionSpec{
		Argv:  []string{t.helper, helperArgv},
		Stdin: true,
		Opts: sandbox.ExecOptions{
			Permission: &permission.Policy{Writes: permission.Writes{Paths: t.writes}},
		},
	})
	if err != nil {
		return fmt.Errorf("fsx: start helper: %w", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Write(ctx, payload); err != nil {
		return fmt.Errorf("fsx: send helper request: %w", err)
	}
	if err := sess.CloseInput(); err != nil {
		return fmt.Errorf("fsx: close helper stdin: %w", err)
	}

	out, err := drain(ctx, sess)
	if err != nil {
		return err
	}
	exit, waitErr := sess.Wait(ctx)
	if waitErr != nil {
		return fmt.Errorf("fsx: helper wait: %w", waitErr)
	}

	resp, err := decodeResponse(out)
	if err != nil {
		return fmt.Errorf("fsx: helper protocol (exit %d): %w", exit.Code, err)
	}
	return resp.err()
}

// drain reads everything the helper writes to stdout.
func drain(ctx context.Context, sess sandbox.Session) ([]byte, error) {
	var out []byte
	after := int64(0)
	for {
		result, err := sess.Read(ctx, after, 1<<20)
		if err != nil {
			return nil, errdefs.FromContext(err)
		}
		for _, chunk := range result.Chunks {
			out = append(out, chunk.Data...)
		}
		after = result.NextSeq
		if result.EOF {
			return out, nil
		}
	}
}

var _ = errors.Is
