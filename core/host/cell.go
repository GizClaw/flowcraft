package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
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
	// journal observes writes. Nil when the backend offers none, which is a fact and not an
	// error: a backend that cannot watch is still a backend.
	journal sandbox.FileJournal
}

// openCell builds one session's boundary from the deployment's backend and the grant.
//
// The order is the dependency order — runner, then the tree, then the journal over the runner —
// and close runs it backwards. A confine the backend cannot honour is refused here, at open, so an
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

	if provider, ok := runner.(sandbox.JournalProvider); ok {
		journal, err := provider.OpenJournal(ctx)
		if err != nil {
			// A backend that cannot attach a journal is usable without one — the tools still work and
			// the boundary is still enforced — but a session running unobserved must not be silent.
			telemetry.WarnErr(ctx, "host: the backend attached no journal", err)
			c.journal = nil
		} else {
			c.journal = journal
		}
	}
	return c, nil
}

// confineOf projects the grant onto the backend's construction-time posture.
//
// It is a projection, not a second spelling: every field comes from the grant unchanged, and
// nothing here decides anything the grant did not.
func confineOf(grant policy.Grant) (sandbox.Confine, error) {
	if err := grant.Files.Validate(); err != nil {
		return sandbox.Confine{}, err
	}
	if err := grant.Limits.Validate(); err != nil {
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

// Close releases the boundary: the journal first (it reads what the runner produces), then the file
// handles, then the runner that could still be spawning processes.
func (c *cell) Close() error {
	var errs []error
	if c.journal != nil {
		if err := c.journal.Close(); err != nil {
			errs = append(errs, fmt.Errorf("host: closing journal: %w", err))
		}
		c.journal = nil
	}
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
