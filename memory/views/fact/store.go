package fact

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const ledgerErrPrefix = "memory/views/fact/ledger"

// ListOptions controls deterministic fact scans.
type ListOptions struct {
	AfterID    FactID
	Limit      int
	Scope      views.Scope
	Subject    string
	Predicate  string
	ActiveOnly bool
	Status     *FactStatus
}

// Store persists fact ledger records.
type Store interface {
	Put(ctx context.Context, fact Fact) (Fact, error)
	Get(ctx context.Context, id FactID) (Fact, bool, error)
	List(ctx context.Context, opts ListOptions) ([]Fact, error)
	Delete(ctx context.Context, id FactID) error
	DeleteSubject(ctx context.Context, subject string) error
}

// Validate checks whether status is one of the supported fact lifecycle states.
func (s FactStatus) Validate() error {
	switch s {
	case FactActive, FactSuperseded, FactRetracted, FactConflict:
		return nil
	default:
		return errdefs.Validationf("%s: unsupported fact status %q", ledgerErrPrefix, s)
	}
}

func validateFact(fact Fact) error {
	if fact.ID == "" {
		return errdefs.Validationf("%s: fact id is required", ledgerErrPrefix)
	}
	if err := fact.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact scope: %w", ledgerErrPrefix, err)
	}
	if fact.Subject == "" {
		return errdefs.Validationf("%s: subject is required", ledgerErrPrefix)
	}
	if fact.Predicate == "" {
		return errdefs.Validationf("%s: predicate is required", ledgerErrPrefix)
	}
	if fact.Object == "" {
		return errdefs.Validationf("%s: object is required", ledgerErrPrefix)
	}
	if err := fact.Status.Validate(); err != nil {
		return err
	}
	if fact.Confidence != 0 && (fact.Confidence < 0 || fact.Confidence > 1) {
		return errdefs.Validationf("%s: confidence must be between 0 and 1", ledgerErrPrefix)
	}
	if fact.ValidFrom != nil && fact.ValidUntil != nil && fact.ValidUntil.Before(*fact.ValidFrom) {
		return errdefs.Validationf("%s: valid_until must be greater than or equal to valid_from", ledgerErrPrefix)
	}
	if fact.RetractedAt != nil && fact.ResolvedAt != nil && fact.ResolvedAt.Before(*fact.RetractedAt) {
		return errdefs.Validationf("%s: resolved_at must be greater than or equal to retracted_at", ledgerErrPrefix)
	}
	if err := validateFactIDs("supersedes", fact.Supersedes); err != nil {
		return err
	}
	if err := validateFactIDs("superseded_by", fact.SupersededBy); err != nil {
		return err
	}
	if err := validateFactIDs("conflict_with", fact.ConflictWith); err != nil {
		return err
	}
	if len(fact.ObservationRefs) == 0 {
		return errdefs.Validationf("%s: observation_refs are required", ledgerErrPrefix)
	}
	for _, ref := range fact.ObservationRefs {
		if err := validateObservationRef(ref); err != nil {
			return err
		}
	}
	for _, ref := range fact.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	if fact.Signature.IsZero() {
		return errdefs.Validationf("%s: signature is required", ledgerErrPrefix)
	}
	if len(fact.Signature.UpstreamViewRefs) == 0 {
		return errdefs.Validationf("%s: upstream observation view refs are required", ledgerErrPrefix)
	}
	if err := fact.Signature.Validate(); err != nil {
		return err
	}
	return nil
}

func validateFactIDs(name string, ids []FactID) error {
	for _, id := range ids {
		if id == "" {
			return errdefs.Validationf("%s: %s fact id is required", ledgerErrPrefix, name)
		}
	}
	return nil
}

func validateObservationRef(ref ObservationRef) error {
	if ref.ObservationID == "" {
		return errdefs.Validationf("%s: observation id is required", ledgerErrPrefix)
	}
	if (ref.ScopeKind == "") != (ref.ScopeID == "") {
		return errdefs.Validationf("%s: observation scope kind and id must be provided together", ledgerErrPrefix)
	}
	return nil
}

func validateFactID(id FactID) error {
	if id == "" {
		return errdefs.Validationf("%s: fact id is required", ledgerErrPrefix)
	}
	return nil
}

func validateSubject(subject string) error {
	if subject == "" {
		return errdefs.Validationf("%s: subject is required", ledgerErrPrefix)
	}
	return nil
}

func validateListOptions(opts ListOptions) error {
	if !opts.Scope.IsZero() {
		if err := opts.Scope.Validate(); err != nil {
			return errdefs.Validationf("%s: invalid list scope: %w", ledgerErrPrefix, err)
		}
	}
	if opts.Status != nil {
		if err := opts.Status.Validate(); err != nil {
			return err
		}
		if opts.ActiveOnly && *opts.Status != FactActive {
			return errdefs.Validationf("%s: active_only cannot be combined with status %q", ledgerErrPrefix, *opts.Status)
		}
	}
	return nil
}

func normalizeListOptions(opts ListOptions) ListOptions {
	if opts.ActiveOnly && opts.Status == nil {
		status := FactActive
		opts.Status = &status
	}
	if opts.Status != nil && *opts.Status == "" {
		status := FactActive
		opts.Status = &status
	}
	return opts
}

func cloneListOptions(in ListOptions) ListOptions {
	out := in
	if in.Status != nil {
		status := *in.Status
		out.Status = &status
	}
	return out
}
