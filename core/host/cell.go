package host

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// cell is one session's boundary: the runner its commands start in, the file handles its tools
// read and write through, and the journal that observes both.
//
// It is built per session and never shared. Sharing an isolation unit contradicts the unit being
// an isolation unit: two conversations would then see each other's processes, and a per-call
// narrowing of what one of them may write would be the only thing keeping them apart — which is a
// decision that belongs to construction, not to every call.
type cell struct {
	// runner is the backend instance this session's commands run in. It is built from the
	// session's confine, so what it enforces is what the grant allowed.
	runner sandbox.Runner
	// tree is the file boundary: the read roots as open handles, plus the write set. A write
	// outside it is refused by the kernel handle, not by a string comparison.
	tree *fsx.Tree
}

// openCell builds one session's boundary from the deployment's backend and the grant.
//
// The order is the dependency order — runner, then the tree over it — and close runs it backwards. A confine the backend cannot honour is refused here, at open, so an
// operator learns at the first session rather than from a model's runtime error.
func openCell(ctx context.Context, factory BackendFactory, grant policy.Grant) (*cell, error) {
	confine, err := confineOf(grant)
	if err != nil {
		return nil, err
	}
	runner, err := factory(confine)
	if err != nil {
		return nil, fmt.Errorf("host: sandbox: %w", err)
	}
	if runner == nil {
		return nil, errors.New("host: sandbox: the backend built nothing")
	}
	c := &cell{runner: runner}
	// The backend validates its own confine at construction; this repeats the check at the layer
	// that owns the session, so a backend that skipped it cannot hand a session a boundary it does
	// not enforce.
	if err := sandbox.ValidateConfine(confine, runner.Capabilities().Policy); err != nil {
		return nil, errors.Join(fmt.Errorf("host: sandbox: %w", err), c.Close())
	}

	tree, err := openTree(grant.Files)
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.tree = tree
	return c, nil
}

// widenedSession is a command running in a cell built for one widened grant.
//
// Closing it closes the session and then the cell, which is the whole point: the boundary the
// command was let out of exists only for that command, so it cannot outlive the session that used
// it. Closing twice is a no-op, because a caller that closes what it was handed and then unwinds
// an error must not close the backend under a live session.
type widenedSession struct {
	sandbox.Session
	cell *cell
	// cancel releases the temporary cell's deadline; it is a no-op when the declaration has no
	// lifetime to apply.
	cancel context.CancelFunc
	once   sync.Once
	close  error
}

// Close releases the session first — it holds a process the runner knows about — and then the
// boundary that process was confined by.
func (s *widenedSession) Close() error {
	s.once.Do(func() {
		s.close = errors.Join(
			closeFailure("host: closing the widened session", s.Session.Close()),
			s.cell.Close())
		if s.cancel != nil {
			s.cancel()
		}
	})
	return s.close
}

// temporaryCell starts spec in a cell built from a widened grant and hands back the session, with
// the cell's lifetime tied to it.
//
// It is one function rather than an open/start/defer-close sequence so the close cannot be
// forgotten at a second call site, and so the failure path is closed too: a command that never
// started — a confine the backend refuses, a cancelled context, a backend that panics while
// starting — leaves no cell behind. The caller owns the returned session and its Close is what
// ends the widened boundary.
func temporaryCell(ctx context.Context, cancel context.CancelFunc, factory BackendFactory, grant policy.Grant, spec sandbox.SessionSpec) (session sandbox.Session, err error) {
	built, err := openCell(ctx, factory, grant)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	defer func() {
		// A panic unwinds through here with session still nil, which is why the condition is
		// "nothing was handed back" and not "an error was returned".
		if session == nil {
			err = errors.Join(err, built.Close())
			if cancel != nil {
				cancel()
			}
		}
	}()
	started, err := built.runner.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	if started == nil {
		// A backend that returns neither a session nor an error has not started anything; the
		// cell is all there is to release.
		return nil, errors.New("host: sandbox: the widened cell started nothing")
	}
	return &widenedSession{Session: started, cell: built, cancel: cancel}, nil
}

// closeFailure names a release failure without replacing the cause.
func closeFailure(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}

// confineOf projects the grant onto the backend's construction-time posture.
//
// It is a projection, not a second spelling: every field comes from the grant unchanged, and
// nothing here decides anything the grant did not.
func confineOf(grant policy.Grant) (sandbox.Confine, error) {
	if err := grant.Files.Validate(); err != nil {
		return sandbox.Confine{}, err
	}
	return sandbox.Confine{
		Files:     grant.Files,
		Net:       grant.Net,
		Env:       grant.Exec.Env,
		Resources: grant.Resources,
	}, nil
}

// openTree opens the file boundary.
//
// The three states of a [policy.Files] map onto the three ways fsx can be told what to allow: a
// read-only grant gets no write handles, a write set gets exactly those roots, and an empty
// Files means both sides are unrestricted — which is the difference between "unlimited" and
// "none", and the only one fsx needs to hear.
func openTree(files policy.Files) (*fsx.Tree, error) {
	switch {
	case files.ReadOnly:
		tree, err := fsx.Open(files.Read)
		if err != nil {
			return nil, fmt.Errorf("host: files: %w", err)
		}
		return tree, nil
	case len(files.Write) > 0:
		tree, err := fsx.Open(files.Read, fsx.WithWrite(files.Write...))
		if err != nil {
			return nil, fmt.Errorf("host: files: %w", err)
		}
		return tree, nil
	default:
		tree, err := fsx.Open(files.Read, fsx.WithAllWrites())
		if err != nil {
			return nil, fmt.Errorf("host: files: %w", err)
		}
		return tree, nil
	}
}

// Close releases the boundary: the file handles first, then the runner that could still be spawning
// processes.
func (c *cell) Close() error {
	var errs []error
	if c.tree != nil {
		if err := c.tree.Close(); err != nil {
			errs = append(errs, fmt.Errorf("host: closing files: %w", err))
		}
		c.tree = nil
	}
	if c.runner != nil {
		if err := c.runner.Close(); err != nil {
			errs = append(errs, fmt.Errorf("host: closing sandbox: %w", err))
		}
		c.runner = nil
	}
	return errors.Join(errs...)
}
