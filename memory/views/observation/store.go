package observation

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const ledgerErrPrefix = "memory/views/observation/ledger"

// ListOptions controls deterministic observation scans.
type ListOptions struct {
	AfterID string
	Limit   int
	Scope   *Scope
	Subject string
}

// Store persists observation ledger records.
type Store interface {
	Put(ctx context.Context, observation Observation) (Observation, error)
	Get(ctx context.Context, id string) (Observation, bool, error)
	List(ctx context.Context, opts ListOptions) ([]Observation, error)
	Delete(ctx context.Context, id string) error
	DeleteScope(ctx context.Context, scope Scope) error
}

func validateObservation(observation Observation) error {
	if observation.ID == "" {
		return errdefs.Validationf("%s: observation id is required", ledgerErrPrefix)
	}
	if err := validateScope(observation.Scope); err != nil {
		return err
	}
	if observation.Subject == "" {
		return errdefs.Validationf("%s: subject is required", ledgerErrPrefix)
	}
	if observation.Predicate == "" {
		return errdefs.Validationf("%s: predicate is required", ledgerErrPrefix)
	}
	if observation.Object == "" {
		return errdefs.Validationf("%s: object is required", ledgerErrPrefix)
	}
	if observation.Confidence != 0 && (observation.Confidence < 0 || observation.Confidence > 1) {
		return errdefs.Validationf("%s: confidence must be between 0 and 1", ledgerErrPrefix)
	}
	if len(observation.SourceRefs) == 0 {
		return errdefs.Validationf("%s: source_refs are required", ledgerErrPrefix)
	}
	for _, ref := range observation.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	if len(observation.Signature.SourceRevisions) == 0 {
		return errdefs.Validationf("%s: source revisions are required", ledgerErrPrefix)
	}
	if len(observation.Signature.UpstreamViewRefs) > 0 {
		return errdefs.Validationf("%s: upstream view refs are not part of observation ledger lineage", ledgerErrPrefix)
	}
	if err := observation.Signature.Validate(); err != nil {
		return err
	}
	return nil
}

func validateScope(scope Scope) error {
	if err := scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid scope: %w", ledgerErrPrefix, err)
	}
	return nil
}

func validateListOptions(opts ListOptions) error {
	if opts.Scope != nil {
		return validateScope(*opts.Scope)
	}
	return nil
}

func cloneListOptions(in ListOptions) ListOptions {
	out := in
	if in.Scope != nil {
		scope := *in.Scope
		out.Scope = &scope
	}
	return out
}

func validateObservationID(id string) error {
	if id == "" {
		return errdefs.Validationf("%s: observation id is required", ledgerErrPrefix)
	}
	return nil
}
